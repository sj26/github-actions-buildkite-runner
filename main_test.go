package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"
)

func TestDemandReplayAcrossRestart(t *testing.T) {
	s := &toy{config: config{StatePath: filepath.Join(t.TempDir(), "state.json"), MaxRunners: 2, MaxBuilds: 2},
		state:    state{Runners: []*runner{{Name: "one", Stage: "running", SecretDeleted: true}, {Name: "two", Stage: "running", SecretDeleted: true}}},
		listener: &listener.Listener{}}
	msg := &scaleset.RunnerScaleSetMessage{MessageID: 42, Statistics: &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 2}}
	for range 2 {
		if err := s.Scale(context.Background(), msg); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(s.config.StatePath)
		if err != nil {
			t.Fatal(err)
		}
		s.state = state{}
		if err := json.Unmarshal(data, &s.state); err != nil {
			t.Fatal(err)
		}
	}
	msg.Statistics.TotalAssignedJobs = 1
	msg.JobCompletedMessages = []*scaleset.JobCompleted{{RunnerName: "one"}}
	for range 2 {
		if err := s.Scale(context.Background(), msg); err != nil {
			t.Fatal(err)
		}
	}
	if len(s.state.Runners) != 2 || s.active() != 1 || s.state.Runners[1].Completed != nil {
		t.Fatal("replay changed runner capacity")
	}
	// A missing build result is not permission to make a replacement.
	s.state.Runners[1].Stage = "creating-build"
	if err := s.Scale(context.Background(), msg); err == nil {
		t.Fatal("uncertain build was retried")
	}
	s.state.Runners[1].Stage = "running"
	s.state.Runners[1].Completed = &scaleset.JobCompleted{RunnerName: "two"}
	if err := s.Scale(context.Background(), msg); err == nil {
		t.Fatal("spent budget allowed another runner")
	}
}

func TestBuildkiteProvisionContract(t *testing.T) {
	var mutations []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer write-token" {
			t.Error("wrong write authority")
		}
		raw, _ := io.ReadAll(r.Body)
		switch r.Method + " " + r.URL.Path {
		case "POST /organizations/example/clusters/cluster/secrets":
			var body map[string]string
			_ = json.Unmarshal(raw, &body)
			if body["value"] != "private-jit" || body["key"] != "one-job" || body["policy"] != "- pipeline_id: \"pipeline\"\n  build_branch: \"trusted\"\n" {
				t.Errorf("wrong secret contract: %s", raw)
			}
			mutations = append(mutations, "secret")
			fmt.Fprint(w, `{"id":"secret-id"}`)
		case "POST /organizations/example/pipelines/runner/builds":
			if strings.Contains(string(raw), "private-jit") || strings.Contains(string(raw), "write-token") {
				t.Error("credential leaked into build")
			}
			var body struct {
				Commit, Branch string
				Env            map[string]string
			}
			_ = json.Unmarshal(raw, &body)
			if body.Commit != "pinned" || body.Branch != "trusted" || body.Env["GITHUB_RUNNER_JIT_SECRET"] != "one-job" || body.Env["RUNNER_QUEUE"] != "small" {
				t.Errorf("wrong build contract: %s", raw)
			}
			mutations = append(mutations, "build")
			fmt.Fprint(w, `{"number":17,"web_url":"https://buildkite.com/example/runner/builds/17"}`)
		case "GET /organizations/example/clusters/cluster/secrets/secret-id":
			fmt.Fprint(w, `{"key":"one-job"}`)
		case "DELETE /organizations/example/clusters/cluster/secrets/secret-id":
			mutations = append(mutations, "delete")
			w.WriteHeader(204)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	b := &buildkite{base: server.URL, http: server.Client(), clusterID: "cluster", pipelineID: "pipeline", config: config{Org: "example", Pipeline: "runner", Queue: "small", Commit: "pinned", Branch: "trusted", writeToken: "write-token"}}
	r := &runner{Name: "buildkite-one", SecretKey: "one-job"}
	var err error
	r.SecretID, err = b.createSecret(context.Background(), r.SecretKey, "private-jit")
	if err != nil {
		t.Fatal(err)
	}
	r.Build, err = b.createBuild(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	if r.Build.Number != 17 {
		t.Fatal("lost build identity")
	}
	if err := b.deleteSecret(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if strings.Join(mutations, ",") != "secret,build,delete" {
		t.Fatal(mutations)
	}
}

func TestUncertainBuildResponseIsNotRetriedOrLogged(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(503)
		fmt.Fprint(w, "private-jit write-token")
	}))
	defer server.Close()
	b := &buildkite{base: server.URL, http: server.Client()}
	_, err := b.createBuild(context.Background(), &runner{Name: "one"})
	if calls != 1 || err == nil || strings.Contains(err.Error(), "private-jit") || strings.Contains(err.Error(), "write-token") {
		t.Fatalf("unsafe failure: calls=%d err=%v", calls, err)
	}
}

func TestSeparateConfigurationAndState(t *testing.T) {
	for k, v := range map[string]string{"GITHUB_CONFIG_URL": "https://github.com/example/repo", "GITHUB_TOKEN": "github", "BUILDKITE_ORG": "example", "BUILDKITE_PIPELINE": "runner", "BUILDKITE_QUEUE": "small", "BUILDKITE_API_TOKEN": "write", "SCALE_SET_NAME": "one", "STATE_PATH": filepath.Join(t.TempDir(), "one.json"), "MAX_BUILDS": "2"} {
		t.Setenv(k, v)
	}
	a, err := configuration()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("SCALE_SET_NAME", "two")
	b, err := configuration()
	if err != nil {
		t.Fatal(err)
	}
	if a.target() == b.target() {
		t.Fatal("scale sets share an identity")
	}
	s := toy{config: a, state: state{Target: a.target()}}
	if err := s.save(); err != nil {
		t.Fatal(err)
	}
	if err := run(); err == nil || !strings.Contains(err.Error(), "different configuration") {
		t.Fatalf("reused another instance's state: %v", err)
	}
}
