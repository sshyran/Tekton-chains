//go:build e2e
// +build e2e

/*
Copyright 2019 The Tekton Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package test

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/storage"
	"github.com/tektoncd/chains/pkg/chains/objects"
	chainsstorage "github.com/tektoncd/chains/pkg/chains/storage"
	"github.com/tektoncd/chains/pkg/config"
	"github.com/tektoncd/chains/pkg/test/tekton"
	"github.com/tektoncd/pipeline/pkg/apis/pipeline/v1beta1"
	pipelineclientset "github.com/tektoncd/pipeline/pkg/client/clientset/versioned"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"knative.dev/pkg/logging"
)

func getTr(ctx context.Context, t *testing.T, c pipelineclientset.Interface, name, ns string) (tr *v1beta1.TaskRun) {
	t.Helper()
	tr, err := c.TektonV1beta1().TaskRuns(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Error(err)
	}
	return tr
}

type conditionFn func(obj objects.TektonObject) bool

func waitForCondition(ctx context.Context, t *testing.T, c pipelineclientset.Interface, obj objects.TektonObject, cond conditionFn, timeout time.Duration) objects.TektonObject {
	t.Helper()

	// Do a first quick check before setting the watch
	o, err := tekton.GetObject(t, ctx, c, obj)
	if err != nil {
		t.Errorf("error watching object: %s", err)
	}
	if cond(o) {
		return o
	}

	w, err := tekton.WatchObject(t, ctx, c, obj)
	if err != nil {
		t.Errorf("error watching object: %s", err)
	}

	// Setup a timeout channel
	timeoutChan := make(chan struct{})
	go func() {
		time.Sleep(timeout)
		timeoutChan <- struct{}{}
	}()

	// Wait for the condition to be true or a timeout
	for {
		select {
		case ev := <-w.ResultChan():
			obj, err := objects.NewTektonObject(ev.Object)
			if err != nil {
				t.Errorf("watch result of unrecognized type: %s", err)
			}
			if cond(obj) {
				return obj
			}
		case <-timeoutChan:
			// print logs from the Tekton object on timeout
			printDebugging(t, obj)
			t.Fatal("time out")
		}
	}
}

func successful(obj objects.TektonObject) bool {
	return obj.IsSuccessful()
}

func failed(obj objects.TektonObject) bool {
	failed, ok := obj.GetAnnotations()["chains.tekton.dev/signed"]
	return ok && failed == "failed" && obj.GetAnnotations()["chains.tekton.dev/retries"] == "3"
}

func done(obj objects.TektonObject) bool {
	return obj.IsDone()
}

func signed(obj objects.TektonObject) bool {
	_, ok := obj.GetAnnotations()["chains.tekton.dev/signed"]
	return ok
}

var simpleTaskspec = v1beta1.TaskSpec{
	Steps: []v1beta1.Step{
		{
			Image:  "busybox",
			Script: "echo true",
		},
	},
}

var simpleTaskRun = v1beta1.TaskRun{
	ObjectMeta: metav1.ObjectMeta{
		GenerateName: "test-task-",
	},
	Spec: v1beta1.TaskRunSpec{
		TaskSpec: &simpleTaskspec,
	},
}

func makeBucket(t *testing.T, client *storage.Client) (string, func()) {
	// Make a bucket
	rand.Seed(time.Now().UnixNano())
	testBucketName := fmt.Sprintf("tekton-chains-e2e-%d", rand.Intn(1000))

	ctx := context.Background()
	proj := os.Getenv("GCP_PROJECT_ID")
	if proj == "" {
		t.Skip("Skipping, no GCP_PROJECT_ID env var set")
	}
	if err := client.Bucket(testBucketName).Create(ctx, proj, nil); err != nil {
		t.Fatal(err)
	}
	return testBucketName, func() {
		// We have to remove the bucket using gsutil. The libraries/APIs require you to
		// first list the bucket then delete everything. List operations are eventually-consistent,
		// so this is non-trivial. gsutil takes care of this.
		cmd := exec.Command("gsutil", "ls", "gs://"+testBucketName)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			t.Log(err)
		}
		cmd = exec.Command("gsutil", "rm", "-rf", "gs://"+testBucketName)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			t.Log(err)
		}
	}
}

func readObj(t *testing.T, bucket, name string, client *storage.Client) io.Reader {
	ctx := context.Background()
	reader, err := client.Bucket(bucket).Object(name).NewReader(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return reader
}

func setConfigMap(ctx context.Context, t *testing.T, c *clients, data map[string]string) func() {
	// Change the config to be GCS storage with this bucket.
	// Note(rgreinho): This comment does not look right...
	cm, err := c.KubeClient.CoreV1().ConfigMaps("tekton-chains").Get(ctx, "chains-config", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}

	// Save the old data to reset it after.
	oldData := map[string]string{}
	for k, v := range cm.Data {
		oldData[k] = v
	}

	if cm.Data == nil {
		cm.Data = map[string]string{}
	}

	for k, v := range data {
		cm.Data[k] = v
	}
	cm, err = c.KubeClient.CoreV1().ConfigMaps("tekton-chains").Update(ctx, cm, metav1.UpdateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Second)

	return func() {
		for k := range data {
			delete(cm.Data, k)
		}
		for k, v := range oldData {
			cm.Data[k] = v
		}
		if _, err := c.KubeClient.CoreV1().ConfigMaps("tekton-chains").Update(ctx, cm, metav1.UpdateOptions{}); err != nil {
			t.Log(err)
		}
	}
}

func printDebugging(t *testing.T, obj objects.TektonObject) {
	t.Logf("============================== %s logs ==============================", obj.GetGVK())
	output, _ := exec.Command("tkn", obj.GetGVK(), "logs", "-n", obj.GetNamespace(), obj.GetName()).CombinedOutput()
	t.Log(string(output))

	t.Logf("============================== %s describe ==============================", obj.GetGVK())
	output, _ = exec.Command("tkn", obj.GetGVK(), "describe", "-n", obj.GetNamespace(), obj.GetName()).CombinedOutput()
	t.Log(string(output))

	t.Log("============================== chains controller logs ==============================")
	output, _ = exec.Command("kubectl", "logs", "deploy/tekton-chains-controller", "-n", "tekton-chains").CombinedOutput()
	t.Log(string(output))
}

func verifySignature(ctx context.Context, t *testing.T, c *clients, obj objects.TektonObject) {
	// Retrieve the configuration.
	chainsConfig, err := c.KubeClient.CoreV1().ConfigMaps("tekton-chains").Get(ctx, "chains-config", metav1.GetOptions{})
	if err != nil {
		t.Errorf("error retrieving tekton chains configmap: %s", err)
	}
	cfg, err := config.NewConfigFromConfigMap(chainsConfig)
	if err != nil {
		t.Errorf("error creating tekton chains configuration: %s", err)
	}

	// Prepare the logger.
	logger := logging.FromContext(ctx)

	// Initialize the backend.
	backends, err := chainsstorage.InitializeBackends(ctx, c.PipelineClient, c.KubeClient, logger, *cfg)
	if err != nil {
		t.Errorf("error initializing backends: %s", err)
	}

	var configuredBackends []string
	var key string
	switch obj.GetObject().(type) {
	case *objects.TaskRunObject:
		configuredBackends = cfg.Artifacts.TaskRuns.StorageBackend.List()
		key = fmt.Sprintf("taskrun-%s", obj.GetUID())
	case *objects.PipelineRunObject:
		configuredBackends = cfg.Artifacts.PipelineRuns.StorageBackend.List()
		key = fmt.Sprintf("pipelinerun-%s", obj.GetUID())
	}

	for _, b := range configuredBackends {
		t.Logf("Backend name: %q\n", b)
		backend := backends[b]

		// Initialize the storage options.
		opts := config.StorageOpts{
			ShortKey: key,
		}

		// Let's fetch the signature and body.
		signatures, err := backend.RetrieveSignatures(ctx, obj, opts)
		if err != nil {
			t.Errorf("error retrieving the signature: %s", err)
		}
		payloads, err := backend.RetrievePayloads(ctx, obj, opts)
		if err != nil {
			t.Errorf("error retrieving the payload: %s", err)
		}

		for ref, payload := range payloads {
			for _, signature := range signatures[ref] {
				if err := c.secret.x509priv.VerifySignature(strings.NewReader(signature), strings.NewReader(payload)); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
}
