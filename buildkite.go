package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"time"
)

type buildkite struct {
	config                config
	http                  *http.Client
	base                  string
	pipelineID, clusterID string
}
type build struct {
	Number    int        `json:"number"`
	URL       string     `json:"web_url"`
	State     string     `json:"state"`
	CreatedAt time.Time  `json:"created_at"`
	StartedAt *time.Time `json:"started_at"`
}
type tokenInfo struct {
	Scopes    []string                     `json:"scopes"`
	ExpiresAt time.Time                    `json:"expires_at"`
	User      struct{ Name, Email string } `json:"user"`
}

// No mutation retries, redirects, request-body logging or response-body errors.
func (b *buildkite) request(ctx context.Context, method, path, token string, body, out any) (int, error) {
	var data io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		data = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, b.base+path, data)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("Buildkite %s request failed; outcome may be unknown", method)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, fmt.Errorf("Buildkite %s: HTTP %d", method, resp.StatusCode)
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return resp.StatusCode, fmt.Errorf("Buildkite response could not be decoded; inspect before retrying")
		}
	}
	return resp.StatusCode, nil
}
func (b *buildkite) pipelinePath() string {
	return "/organizations/" + b.config.Org + "/pipelines/" + b.config.Pipeline
}
func (b *buildkite) secretsPath() string {
	return "/organizations/" + b.config.Org + "/clusters/" + b.clusterID + "/secrets"
}
func (b *buildkite) policy() string {
	// JSON-quoted scalars are also valid YAML; the JIT secret only serves this pipeline/branch.
	id, _ := json.Marshal(b.pipelineID)
	branch, _ := json.Marshal(b.config.Branch)
	return fmt.Sprintf("- pipeline_id: %s\n  build_branch: %s\n", id, branch)
}
func (b *buildkite) preflight(ctx context.Context) error {
	var write, read tokenInfo
	if _, err := b.request(ctx, "GET", "/access-token", b.config.writeToken, nil, &write); err != nil {
		return err
	}
	if _, err := b.request(ctx, "GET", "/access-token", b.config.readToken, nil, &read); err != nil {
		return err
	}
	for _, scope := range []string{"write_builds", "write_secrets", "read_secrets_details"} {
		if !slices.Contains(write.Scopes, scope) {
			return fmt.Errorf("BUILDKITE_API_TOKEN needs %s", scope)
		}
	}
	for _, scope := range write.Scopes {
		if len(scope) > 6 && scope[:6] == "write_" && scope != "write_builds" && scope != "write_secrets" {
			return fmt.Errorf("use a token without unrelated write scopes")
		}
	}
	for _, scope := range []string{"read_pipelines", "read_builds"} {
		if !slices.Contains(read.Scopes, scope) {
			return fmt.Errorf("Buildkite read credential needs %s", scope)
		}
	}
	if write.User != read.User {
		return fmt.Errorf("Buildkite read/write credentials must belong to the same identity")
	}
	for _, token := range []tokenInfo{read, write} {
		if token.ExpiresAt.Before(time.Now().Add(b.config.Lifetime + 2*time.Minute)) {
			return fmt.Errorf("Buildkite token expires before this bounded run can finish")
		}
	}
	var pipeline struct {
		ID      string `json:"id"`
		Cluster string `json:"cluster_id"`
		Steps   []struct {
			Type  string   `json:"type"`
			Rules []string `json:"agent_query_rules"`
		} `json:"steps"`
	}
	if _, err := b.request(ctx, "GET", b.pipelinePath(), b.config.readToken, nil, &pipeline); err != nil {
		return err
	}
	if len(pipeline.Steps) != 1 || pipeline.Steps[0].Type != "script" || !slices.Contains(pipeline.Steps[0].Rules, "queue="+b.config.Queue) {
		return fmt.Errorf("pipeline must contain one runner command step on BUILDKITE_QUEUE")
	}
	if pipeline.ID == "" || pipeline.Cluster == "" {
		return fmt.Errorf("pipeline must belong to a cluster")
	}
	b.pipelineID, b.clusterID = pipeline.ID, pipeline.Cluster
	return nil
}
func (b *buildkite) createSecret(ctx context.Context, key, jit string) (string, error) {
	var secret struct {
		ID string `json:"id"`
	}
	_, err := b.request(ctx, "POST", b.secretsPath(), b.config.writeToken, map[string]string{
		"key": key, "value": jit, "policy": b.policy(), "description": "One-job GitHub Actions runner"}, &secret)
	if err == nil && secret.ID == "" {
		err = fmt.Errorf("secret creation outcome unknown")
	}
	return secret.ID, err
}
func (b *buildkite) createBuild(ctx context.Context, r *runner) (build, error) {
	var result build
	_, err := b.request(ctx, "POST", b.pipelinePath()+"/builds", b.config.writeToken, map[string]any{
		"commit": b.config.Commit, "branch": b.config.Branch, "message": "GitHub runner " + r.Name,
		"env":       map[string]string{"GITHUB_RUNNER_JIT_SECRET": r.SecretKey, "RUNNER_QUEUE": b.config.Queue},
		"meta_data": map[string]string{"github_runner": r.Name}}, &result)
	if err == nil && result.Number == 0 {
		err = fmt.Errorf("build creation outcome unknown")
	}
	return result, err
}
func (b *buildkite) getBuild(ctx context.Context, number int) (build, error) {
	var result build
	_, err := b.request(ctx, "GET", fmt.Sprintf("%s/builds/%d", b.pipelinePath(), number), b.config.readToken, nil, &result)
	return result, err
}
func (b *buildkite) deleteSecret(ctx context.Context, r *runner) error {
	path := b.secretsPath() + "/" + r.SecretID
	var secret struct {
		Key string `json:"key"`
	}
	status, err := b.request(ctx, "GET", path, b.config.writeToken, nil, &secret)
	if status == 404 {
		return nil
	}
	if err != nil {
		return err
	}
	if secret.Key != r.SecretKey {
		return fmt.Errorf("refusing to delete a different secret")
	}
	_, err = b.request(ctx, "DELETE", path, b.config.writeToken, nil, nil)
	return err
}
