/*
Copyright 2016 The Kubernetes Authors.

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

package kube

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"strings"
	"time"

	"github.com/ghodss/yaml"
	"github.com/sirupsen/logrus"
	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
)

const (
	inClusterBaseURL = "https://kubernetes.default"
	maxRetries       = 8
	retryDelay       = 2 * time.Second
	requestTimeout   = time.Minute

	EmptySelector = ""

	DefaultClusterAlias = "default"
)

// newClient is used to allow mocking out the behavior of 'NewClient' while testing.
var newClient func(c *Cluster, namespace string) (*Client, error) = NewClient

type Logger interface {
	Debugf(s string, v ...interface{})
}

// Client interacts with the Kubernetes api-server.
type Client struct {
	// If logger is non-nil, log all method calls with it.
	logger Logger

	baseURL   string
	deckURL   string
	client    *http.Client
	token     string
	namespace string
	fake      bool

	hiddenReposProvider func() []string
	hiddenOnly          bool
}

// SetHiddenRepoProvider takes a continuation that fetches a list of orgs and repos for
// which PJs should not be returned.
// NOTE: This function is not thread safe and should be called before the client is in use.
func (c *Client) SetHiddenReposProvider(p func() []string, hiddenOnly bool) {
	c.hiddenReposProvider = p
	c.hiddenOnly = hiddenOnly
}

// Namespace returns a copy of the client pointing at the specified namespace.
func (c *Client) Namespace(ns string) *Client {
	nc := *c
	nc.namespace = ns
	return &nc
}

func (c *Client) log(methodName string, args ...interface{}) {
	if c.logger == nil {
		return
	}
	var as []string
	for _, arg := range args {
		as = append(as, fmt.Sprintf("%v", arg))
	}
	c.logger.Debugf("%s(%s)", methodName, strings.Join(as, ", "))
}

type ConflictError struct {
	e error
}

func (e ConflictError) Error() string {
	return e.e.Error()
}

func NewConflictError(e error) ConflictError {
	return ConflictError{e: e}
}

type UnprocessableEntityError struct {
	e error
}

func (e UnprocessableEntityError) Error() string {
	return e.e.Error()
}

func NewUnprocessableEntityError(e error) UnprocessableEntityError {
	return UnprocessableEntityError{e: e}
}

type request struct {
	method      string
	path        string
	deckPath    string
	query       map[string]string
	requestBody interface{}
}

func (c *Client) request(r *request, ret interface{}) error {
	out, err := c.requestRetry(r)
	if err != nil {
		return err
	}
	if ret != nil {
		if err := json.Unmarshal(out, ret); err != nil {
			return err
		}
	}
	return nil
}

func (c *Client) retry(r *request) (*http.Response, error) {
	var resp *http.Response
	var err error
	backoff := retryDelay
	for retries := 0; retries < maxRetries; retries++ {
		resp, err = c.doRequest(r.method, r.deckPath, r.path, r.query, r.requestBody)
		if err == nil {
			if resp.StatusCode < 500 {
				break
			}
			resp.Body.Close()
		}

		time.Sleep(backoff)
		backoff *= 2
	}
	return resp, err
}

// Retry on transport failures. Does not retry on 500s.
func (c *Client) requestRetryStream(r *request) (io.ReadCloser, error) {
	if c.fake && r.deckPath == "" {
		return nil, nil
	}
	resp, err := c.retry(r)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == 409 {
		return nil, NewConflictError(fmt.Errorf("body cannot be streamed"))
	} else if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("response has status \"%s\"", resp.Status)
	}
	return resp.Body, nil
}

// Retry on transport failures. Does not retry on 500s.
func (c *Client) requestRetry(r *request) ([]byte, error) {
	if c.fake && r.deckPath == "" {
		return []byte("{}"), nil
	}
	resp, err := c.retry(r)
	if err != nil {
		return nil, err
	}

	defer resp.Body.Close()
	rb, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == 409 {
		return nil, NewConflictError(fmt.Errorf("body: %s", string(rb)))
	} else if resp.StatusCode == 422 {
		return nil, NewUnprocessableEntityError(fmt.Errorf("body: %s", string(rb)))
	} else if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("response has status \"%s\" and body \"%s\"", resp.Status, string(rb))
	}
	return rb, nil
}

func (c *Client) doRequest(method, deckPath, urlPath string, query map[string]string, body interface{}) (*http.Response, error) {
	url := c.baseURL + urlPath
	if c.deckURL != "" && deckPath != "" {
		url = c.deckURL + deckPath
	}
	var buf io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		buf = bytes.NewBuffer(b)
	}
	req, err := http.NewRequest(method, url, buf)
	if err != nil {
		return nil, err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if method == http.MethodPatch {
		req.Header.Set("Content-Type", "application/strategic-merge-patch+json")
	} else {
		req.Header.Set("Content-Type", "application/json")
	}

	q := req.URL.Query()
	for k, v := range query {
		q.Add(k, v)
	}
	req.URL.RawQuery = q.Encode()

	return c.client.Do(req)
}

// NewFakeClient creates a client that doesn't do anything. If you provide a
// deck URL then the client will hit that for the supported calls.
func NewFakeClient(deckURL string) *Client {
	return &Client{
		namespace: "default",
		deckURL:   deckURL,
		client:    &http.Client{},
		fake:      true,
	}
}

// NewClientInCluster creates a Client that works from within a pod.
func NewClientInCluster(namespace string) (*Client, error) {
	tokenFile := "/var/run/secrets/kubernetes.io/serviceaccount/token"
	token, err := ioutil.ReadFile(tokenFile)
	if err != nil {
		return nil, err
	}

	rootCAFile := "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
	certData, err := ioutil.ReadFile(rootCAFile)
	if err != nil {
		return nil, err
	}

	cp := x509.NewCertPool()
	cp.AppendCertsFromPEM(certData)

	tr := &http.Transport{
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
			RootCAs:    cp,
		},
	}
	return &Client{
		logger:    logrus.WithField("client", "kube"),
		baseURL:   inClusterBaseURL,
		client:    &http.Client{Transport: tr, Timeout: requestTimeout},
		token:     string(token),
		namespace: namespace,
	}, nil
}

// Cluster represents the information necessary to talk to a Kubernetes
// master endpoint.
// NOTE: if your cluster runs on GKE you can use the following command to get these credentials:
// gcloud --project <gcp_project> container clusters describe --zone <zone> <cluster_name>
type Cluster struct {
	// The IP address of the cluster's master endpoint.
	Endpoint string `yaml:"endpoint"`
	// Base64-encoded public cert used by clients to authenticate to the
	// cluster endpoint.
	ClientCertificate string `yaml:"clientCertificate"`
	// Base64-encoded private key used by clients..
	ClientKey string `yaml:"clientKey"`
	// Base64-encoded public certificate that is the root of trust for the
	// cluster.
	ClusterCACertificate string `yaml:"clusterCaCertificate"`
}

// NewClientFromFile reads a Cluster object at clusterPath and returns an
// authenticated client using the keys within.
func NewClientFromFile(clusterPath, namespace string) (*Client, error) {
	data, err := ioutil.ReadFile(clusterPath)
	if err != nil {
		return nil, err
	}
	var c Cluster
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, err
	}
	return NewClient(&c, namespace)
}

// ClientMapFromFile reads the file at clustersPath and attempts to load a map of cluster aliases
// to authenticated clients to the respective clusters.
// The file at clustersPath is expected to be a yaml map from strings to Cluster structs OR it may
// simply be a single Cluster struct which will be assigned the alias $DefaultClusterAlias.
// If the file is an alias map, it must include the alias $DefaultClusterAlias.
func ClientMapFromFile(clustersPath, namespace string) (map[string]*Client, error) {
	data, err := ioutil.ReadFile(clustersPath)
	if err != nil {
		return nil, err
	}
	var raw map[string]Cluster
	if err := yaml.Unmarshal(data, &raw); err != nil {
		// If we failed to unmarshal the multicluster format try the single Cluster format.
		var singleConfig Cluster
		if err := yaml.Unmarshal(data, &singleConfig); err != nil {
			return nil, err
		}
		raw = map[string]Cluster{DefaultClusterAlias: singleConfig}
	}
	foundDefault := false
	result := map[string]*Client{}
	for alias, config := range raw {
		client, err := newClient(&config, namespace)
		if err != nil {
			return nil, fmt.Errorf("failed to load config for build cluster alias %q in file %q: %v", alias, clustersPath, err)
		}
		result[alias] = client
		if alias == DefaultClusterAlias {
			foundDefault = true
		}
	}
	if !foundDefault {
		return nil, fmt.Errorf("failed to find the required %q alias in build cluster config %q", DefaultClusterAlias, clustersPath)
	}
	return result, nil
}

// NewClient returns an authenticated Client using the keys in the Cluster.
func NewClient(c *Cluster, namespace string) (*Client, error) {
	cc, err := base64.StdEncoding.DecodeString(c.ClientCertificate)
	if err != nil {
		return nil, err
	}
	ck, err := base64.StdEncoding.DecodeString(c.ClientKey)
	if err != nil {
		return nil, err
	}
	ca, err := base64.StdEncoding.DecodeString(c.ClusterCACertificate)
	if err != nil {
		return nil, err
	}

	cert, err := tls.X509KeyPair(cc, ck)
	if err != nil {
		return nil, err
	}

	cp := x509.NewCertPool()
	cp.AppendCertsFromPEM(ca)

	tr := &http.Transport{
		TLSClientConfig: &tls.Config{
			MinVersion:   tls.VersionTLS12,
			Certificates: []tls.Certificate{cert},
			RootCAs:      cp,
		},
	}
	return &Client{
		logger:    logrus.WithField("client", "kube"),
		baseURL:   c.Endpoint,
		client:    &http.Client{Transport: tr, Timeout: requestTimeout},
		namespace: namespace,
	}, nil
}

func (c *Client) GetPod(name string) (Pod, error) {
	c.log("GetPod", name)
	var retPod Pod
	err := c.request(&request{
		path: fmt.Sprintf("/api/v1/namespaces/%s/pods/%s", c.namespace, name),
	}, &retPod)
	return retPod, err
}

func (c *Client) ListPods(selector string) ([]Pod, error) {
	c.log("ListPods", selector)
	var pl struct {
		Items []Pod `json:"items"`
	}
	err := c.request(&request{
		path:  fmt.Sprintf("/api/v1/namespaces/%s/pods", c.namespace),
		query: map[string]string{"labelSelector": selector},
	}, &pl)
	return pl.Items, err
}

func (c *Client) DeletePod(name string) error {
	c.log("DeletePod", name)
	return c.request(&request{
		method: http.MethodDelete,
		path:   fmt.Sprintf("/api/v1/namespaces/%s/pods/%s", c.namespace, name),
	}, nil)
}

func (c *Client) CreateProwJob(j ProwJob) (ProwJob, error) {
	c.log("CreateProwJob", j)
	var retJob ProwJob
	err := c.request(&request{
		method:      http.MethodPost,
		path:        fmt.Sprintf("/apis/prow.k8s.io/v1/namespaces/%s/prowjobs", c.namespace),
		requestBody: &j,
	}, &retJob)
	return retJob, err
}

func (c *Client) getHiddenRepos() sets.String {
	if c.hiddenReposProvider == nil {
		return nil
	}
	return sets.NewString(c.hiddenReposProvider()...)
}

func shouldHide(pj *ProwJob, hiddenRepos sets.String, showHiddenOnly bool) bool {
	shouldHide := hiddenRepos.HasAny(fmt.Sprintf("%s/%s", pj.Spec.Refs.Org, pj.Spec.Refs.Repo), pj.Spec.Refs.Org)
	if showHiddenOnly {
		return !shouldHide
	}
	return shouldHide
}

func (c *Client) GetProwJob(name string) (ProwJob, error) {
	c.log("GetProwJob", name)
	var pj ProwJob
	err := c.request(&request{
		path: fmt.Sprintf("/apis/prow.k8s.io/v1/namespaces/%s/prowjobs/%s", c.namespace, name),
	}, &pj)
	if err == nil && shouldHide(&pj, c.getHiddenRepos(), c.hiddenOnly) {
		pj = ProwJob{}
		// Revealing the existence of this prow job is ok because the the pj name cannot be used to
		// retrieve the pj itself. Furthermore, a timing attack could differentiate true 404s from
		// 404s returned when a hidden pj is queried so returning a 404 wouldn't hide the pj's existence.
		err = errors.New("403 ProwJob is hidden")
	}
	return pj, err
}

func (c *Client) ListProwJobs(selector string) ([]ProwJob, error) {
	c.log("ListProwJobs", selector)
	var jl struct {
		Items []ProwJob `json:"items"`
	}
	err := c.request(&request{
		path:     fmt.Sprintf("/apis/prow.k8s.io/v1/namespaces/%s/prowjobs", c.namespace),
		deckPath: "/prowjobs.js",
		query:    map[string]string{"labelSelector": selector},
	}, &jl)
	if err == nil {
		hidden := c.getHiddenRepos()
		var pjs []ProwJob
		for _, pj := range jl.Items {
			if !shouldHide(&pj, hidden, c.hiddenOnly) {
				pjs = append(pjs, pj)
			}
		}
		jl.Items = pjs
	}
	return jl.Items, err
}

func (c *Client) DeleteProwJob(name string) error {
	c.log("DeleteProwJob", name)
	return c.request(&request{
		method: http.MethodDelete,
		path:   fmt.Sprintf("/apis/prow.k8s.io/v1/namespaces/%s/prowjobs/%s", c.namespace, name),
	}, nil)
}

func (c *Client) ReplaceProwJob(name string, job ProwJob) (ProwJob, error) {
	c.log("ReplaceProwJob", name, job)
	var retJob ProwJob
	err := c.request(&request{
		method:      http.MethodPut,
		path:        fmt.Sprintf("/apis/prow.k8s.io/v1/namespaces/%s/prowjobs/%s", c.namespace, name),
		requestBody: &job,
	}, &retJob)
	return retJob, err
}

func (c *Client) CreatePod(p v1.Pod) (Pod, error) {
	c.log("CreatePod", p)
	var retPod Pod
	err := c.request(&request{
		method:      http.MethodPost,
		path:        fmt.Sprintf("/api/v1/namespaces/%s/pods", c.namespace),
		requestBody: &p,
	}, &retPod)
	return retPod, err
}

func (c *Client) GetLog(pod string) ([]byte, error) {
	c.log("GetLog", pod)
	return c.requestRetry(&request{
		path: fmt.Sprintf("/api/v1/namespaces/%s/pods/%s/log", c.namespace, pod),
	})
}

func (c *Client) GetLogStream(pod string, options map[string]string) (io.ReadCloser, error) {
	c.log("GetLogStream", pod)
	return c.requestRetryStream(&request{
		path:  fmt.Sprintf("/api/v1/namespaces/%s/pods/%s/log", c.namespace, pod),
		query: options,
	})
}

func (c *Client) CreateConfigMap(content ConfigMap) (ConfigMap, error) {
	c.log("CreateConfigMap")
	var retConfigMap ConfigMap
	err := c.request(&request{
		method:      http.MethodPost,
		path:        fmt.Sprintf("/api/v1/namespaces/%s/configmaps", c.namespace),
		requestBody: &content,
	}, &retConfigMap)

	return retConfigMap, err
}

func (c *Client) ReplaceConfigMap(name string, config ConfigMap) (ConfigMap, error) {
	c.log("ReplaceConfigMap", name)
	var retConfigMap ConfigMap
	err := c.request(&request{
		method:      http.MethodPut,
		path:        fmt.Sprintf("/api/v1/namespaces/%s/configmaps/%s", c.namespace, name),
		requestBody: &config,
	}, &retConfigMap)

	return retConfigMap, err
}
