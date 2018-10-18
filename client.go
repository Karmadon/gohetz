package gohetz

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"./models"
)

// Endpoint is the base URL of the API.
const Endpoint = "https://api.hetzner.cloud/v1"

// UserAgent is the value for the library part of the User-Agent header
// that is sent with each request.
const UserAgent = "hcloud-go/" + Version

// A BackoffFunc returns the duration to wait before performing the
// next retry. The retries argument specifies how many retries have
// already been performed. When called for the first time, retries is 0.
type BackoffFunc func(retries int) time.Duration

// ConstantBackoff returns a BackoffFunc which backs off for
// constant duration d.
func ConstantBackoff(d time.Duration) BackoffFunc {
	return func(_ int) time.Duration {
		return d
	}
}

// ExponentialBackoff returns a BackoffFunc which implements an exponential
// backoff using the formula: b^retries * d
func ExponentialBackoff(b float64, d time.Duration) BackoffFunc {
	return func(retries int) time.Duration {
		return time.Duration(math.Pow(b, float64(retries))) * d
	}
}

// Client is a client for the Hetzner Cloud API.
type Client struct {
	endpoint           string
	token              string
	pollInterval       time.Duration
	backoffFunc        BackoffFunc
	httpClient         *http.Client
	applicationName    string
	applicationVersion string
	userAgent          string
}

func (c *Client) GetAllServers() (*models.Servers, error) {

	req, err := c.NewRequest(context.Background(), "GET", fmt.Sprintf("/servers/"), nil)
	if err != nil {
		return nil, err
	}

	raw, err := c.Do(req, nil)
	if err != nil {
		return nil, err
	}

	bodyBytes, err := ioutil.ReadAll(raw.Body)
	if err != nil {
		return nil, err
	}

	servers, err := models.UnmarshalServers(bodyBytes)
	if err != nil {
		return nil, err
	}

	return &servers, nil
}

// A ClientOption is used to configure a Client.
type ClientOption func(*Client)

// WithEndpoint configures a Client to use the specified API endpoint.
func WithEndpoint(endpoint string) ClientOption {
	return func(client *Client) {
		client.endpoint = strings.TrimRight(endpoint, "/")
	}
}

// WithToken configures a Client to use the specified token for authentication.
func WithToken(token string) ClientOption {
	return func(client *Client) {
		client.token = token
	}
}

// WithPollInterval configures a Client to use the specified interval when polling
// from the API.
func WithPollInterval(pollInterval time.Duration) ClientOption {
	return func(client *Client) {
		client.pollInterval = pollInterval
	}
}

// WithBackoffFunc configures a Client to use the specified backoff function.
func WithBackoffFunc(f BackoffFunc) ClientOption {
	return func(client *Client) {
		client.backoffFunc = f
	}
}

// WithApplication configures a Client with the given application name and
// application version. The version may be blank. Programs are encouraged
// to at least set an application name.
func WithApplication(name, version string) ClientOption {
	return func(client *Client) {
		client.applicationName = name
		client.applicationVersion = version
	}
}

// NewClient creates a new client.
func NewClient(options ...ClientOption) *Client {
	client := &Client{
		endpoint:     Endpoint,
		httpClient:   &http.Client{},
		backoffFunc:  ExponentialBackoff(2, 500*time.Millisecond),
		pollInterval: 500 * time.Millisecond,
	}

	for _, option := range options {
		option(client)
	}

	client.buildUserAgent()

	return client
}

// NewRequest creates an HTTP request against the API. The returned request
// is assigned with ctx and has all necessary headers set (auth, user agent, etc.).
func (c *Client) NewRequest(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	url := c.endpoint + path
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", c.token))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req = req.WithContext(ctx)
	return req, nil
}

// Do performs an HTTP request against the API.
func (c *Client) Do(r *http.Request, v interface{}) (*Response, error) {
	var retries int
	for {
		resp, err := c.httpClient.Do(r)
		if err != nil {
			return nil, err
		}
		response := &Response{Response: resp}

		body, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			resp.Body.Close()
			return response, err
		}
		resp.Body.Close()
		resp.Body = ioutil.NopCloser(bytes.NewReader(body))

		if err = response.readMeta(body); err != nil {
			return response, fmt.Errorf("hcloud: error reading response meta data: %s", err)
		}

		if resp.StatusCode >= 400 && resp.StatusCode <= 599 {
			err = errorFromResponse(resp, body)
			if err == nil {
				err = fmt.Errorf("hcloud: server responded with status code %d", resp.StatusCode)
			} else {
				if err, ok := err.(Error); ok && err.Code == ErrorCodeRateLimitExceeded {
					c.backoff(retries)
					retries++
					continue
				}
			}
			return response, err
		}
		if v != nil {
			if w, ok := v.(io.Writer); ok {
				_, err = io.Copy(w, bytes.NewReader(body))
			} else {
				err = json.Unmarshal(body, v)
			}
		}

		return response, err
	}
}

func (c *Client) backoff(retries int) {
	time.Sleep(c.backoffFunc(retries))
}

func (c *Client) all(f func(int) (*Response, error)) (*Response, error) {
	var (
		page = 1
	)
	for {
		resp, err := f(page)
		if err != nil {
			return nil, err
		}
		if resp.Meta.Pagination == nil || resp.Meta.Pagination.NextPage == 0 {
			return resp, nil
		}
		page = resp.Meta.Pagination.NextPage
	}
}

func (c *Client) buildUserAgent() {
	switch {
	case c.applicationName != "" && c.applicationVersion != "":
		c.userAgent = c.applicationName + "/" + c.applicationVersion + " " + UserAgent
	case c.applicationName != "" && c.applicationVersion == "":
		c.userAgent = c.applicationName + " " + UserAgent
	default:
		c.userAgent = UserAgent
	}
}

func errorFromResponse(resp *http.Response, body []byte) error {
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
		return nil
	}

	var respBody models.ErrorResponse
	if err := json.Unmarshal(body, &respBody); err != nil {
		return nil
	}
	if respBody.Error.Code == "" && respBody.Error.Message == "" {
		return nil
	}
	return ErrorFromSchema(respBody.Error)
}

// Response represents a response from the API. It embeds http.Response.
type Response struct {
	*http.Response
	Meta Meta
}

func (r *Response) readMeta(body []byte) error {
	if h := r.Header.Get("RateLimit-Limit"); h != "" {
		r.Meta.Ratelimit.Limit, _ = strconv.Atoi(h)
	}
	if h := r.Header.Get("RateLimit-Remaining"); h != "" {
		r.Meta.Ratelimit.Remaining, _ = strconv.Atoi(h)
	}
	if h := r.Header.Get("RateLimit-Reset"); h != "" {
		if ts, err := strconv.ParseInt(h, 10, 64); err == nil {
			r.Meta.Ratelimit.Reset = time.Unix(ts, 0)
		}
	}

	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		var s models.MetaResponse
		if err := json.Unmarshal(body, &s); err != nil {
			return err
		}
		if s.Meta.Pagination != nil {
			p := PaginationFromSchema(*s.Meta.Pagination)
			r.Meta.Pagination = &p
		}
	}

	return nil
}

// Meta represents meta information included in an API response.
type Meta struct {
	Pagination *Pagination
	Ratelimit  Ratelimit
}

// Pagination represents pagination meta information.
type Pagination struct {
	Page         int
	PerPage      int
	PreviousPage int
	NextPage     int
	LastPage     int
	TotalEntries int
}

// PaginationFromSchema converts a schema.MetaPagination to a Pagination.
func PaginationFromSchema(s models.MetaPagination) Pagination {
	return Pagination{
		Page:         s.Page,
		PerPage:      s.PerPage,
		PreviousPage: s.PreviousPage,
		NextPage:     s.NextPage,
		LastPage:     s.LastPage,
		TotalEntries: s.TotalEntries,
	}
}

// Ratelimit represents ratelimit information.
type Ratelimit struct {
	Limit     int
	Remaining int
	Reset     time.Time
}

// ListOpts specifies options for listing resources.
type ListOpts struct {
	Page          int    // Page (starting at 1)
	PerPage       int    // Items per page (0 means default)
	LabelSelector string // Label selector for filtering by labels
}

func valuesForListOpts(opts ListOpts) url.Values {
	vals := url.Values{}
	if opts.Page > 0 {
		vals.Add("page", strconv.Itoa(opts.Page))
	}
	if opts.PerPage > 0 {
		vals.Add("per_page", strconv.Itoa(opts.PerPage))
	}
	if len(opts.LabelSelector) > 0 {
		vals.Add("label_selector", opts.LabelSelector)
	}
	return vals
}

// ErrorFromSchema converts a schema.Error to an Error.
func ErrorFromSchema(s models.Error) Error {
	e := Error{
		Code:    ErrorCode(s.Code),
		Message: s.Message,
	}

	switch d := s.Details.(type) {
	case models.ErrorDetailsInvalidInput:
		details := ErrorDetailsInvalidInput{
			Fields: []ErrorDetailsInvalidInputField{},
		}
		for _, field := range d.Fields {
			details.Fields = append(details.Fields, ErrorDetailsInvalidInputField{
				Name:     field.Name,
				Messages: field.Messages,
			})
		}
		e.Details = details
	}
	return e
}
