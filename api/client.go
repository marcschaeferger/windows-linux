package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/fosrl/newt/logger"
	"github.com/fosrl/windows-linux/version"
)

// APIError represents an error from the API client
type APIError struct {
	Type    ErrorType
	Status  int
	Message string
	Err     error
}

type ErrorType int

const (
	ErrorTypeInvalidURL ErrorType = iota
	ErrorTypeInvalidResponse
	ErrorTypeHTTPError
	ErrorTypeNetworkError
	ErrorTypeDecodingError
)

func (e *APIError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	switch e.Type {
	case ErrorTypeInvalidURL:
		return "Invalid URL"
	case ErrorTypeInvalidResponse:
		return "Invalid response from server"
	case ErrorTypeHTTPError:
		if e.Status != 0 {
			return fmt.Sprintf("HTTP error %d", e.Status)
		}
		return "HTTP error"
	case ErrorTypeNetworkError:
		if e.Err != nil {
			return e.Err.Error()
		}
		return "Network error"
	case ErrorTypeDecodingError:
		if e.Err != nil {
			return fmt.Sprintf("Failed to decode response: %v", e.Err)
		}
		return "Failed to decode response"
	default:
		return "Unknown error"
	}
}

func (e *APIError) Unwrap() error {
	return e.Err
}

// APIClient handles HTTP requests to the Pangolin API.
//
// The base URL and session token change on account switch, login and logout
// while requests run on other goroutines, so they are guarded by mu and each
// request works from one snapshot of them.
type APIClient struct {
	mu                sync.RWMutex
	baseURL           string
	sessionToken      string
	sessionCookieName string
	csrfToken         string
	client            *http.Client
	onUnauthorized    func(token string)
}

// session is the part of the client a single request is built from.
type session struct {
	baseURL    string
	token      string
	cookieName string
}

func (c *APIClient) snapshot() session {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return session{baseURL: c.baseURL, token: c.sessionToken, cookieName: c.sessionCookieName}
}

// NewAPIClient creates a new API client instance
func NewAPIClient(baseURL string, sessionToken string) *APIClient {
	normalizedURL := normalizeBaseURL(baseURL)

	client := &http.Client{
		Timeout:       30 * time.Second,
		CheckRedirect: checkRedirect,
	}

	apiClient := &APIClient{
		baseURL:           normalizedURL,
		sessionToken:      sessionToken,
		sessionCookieName: "p_session_token",
		csrfToken:         "x-csrf-protection",
		client:            client,
	}

	logger.Info("APIClient initialized with baseURL: %s", apiClient.baseURL)
	return apiClient
}

// SetSessionCookieName overrides the cookie name the session token is sent and
// read under. An empty name keeps the default.
func (c *APIClient) SetSessionCookieName(name string) {
	if name = strings.TrimSpace(name); name != "" {
		c.mu.Lock()
		c.sessionCookieName = name
		c.mu.Unlock()
	}
}

// SessionCookieName returns the cookie name the session token is sent under.
func (c *APIClient) SessionCookieName() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.sessionCookieName
}

// UpdateBaseURL updates the base URL for the API client
func (c *APIClient) UpdateBaseURL(newBaseURL string) {
	c.mu.Lock()
	c.baseURL = normalizeBaseURL(newBaseURL)
	c.mu.Unlock()
}

// UpdateSessionToken updates the session token
func (c *APIClient) UpdateSessionToken(token string) {
	c.mu.Lock()
	c.sessionToken = token
	c.mu.Unlock()
}

// SetSession points the client at another server and session in one step, so
// no request is ever sent with the new server and the old token.
func (c *APIClient) SetSession(baseURL, token string) {
	c.mu.Lock()
	c.baseURL = normalizeBaseURL(baseURL)
	c.sessionToken = token
	c.mu.Unlock()
}

// CurrentBaseURL returns the current base URL
func (c *APIClient) CurrentBaseURL() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.baseURL
}

// SessionToken returns the session token requests are currently sent with.
func (c *APIClient) SessionToken() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.sessionToken
}

// SetOnUnauthorized sets the callback invoked when a request sent with a session
// token returns 401. It gets the token the request was sent with.
func (c *APIClient) SetOnUnauthorized(fn func(token string)) {
	c.mu.Lock()
	c.onUnauthorized = fn
	c.mu.Unlock()
}

// checkRedirect follows redirects like the default policy, except that it
// refuses one that would turn a POST (or any non-GET request) into a GET. Go
// does that on 301, 302 and 303, which drops the request body: a login POST to
// an http:// server that redirects to https would reach the server as a GET
// and fail with a confusing "Not found".
func checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	orig := via[0]
	if req.Method != orig.Method {
		if orig.URL.Scheme == "http" && req.URL.Scheme == "https" {
			return fmt.Errorf("the server redirects to https://%s; use https:// in the server address", req.URL.Host)
		}
		return fmt.Errorf("the server redirected a %s request to %s", orig.Method, redactURL(req.URL))
	}
	return nil
}

// redactURL is u without its query string, which may carry a device code.
func redactURL(u *url.URL) string {
	r := *u
	r.RawQuery = ""
	r.User = nil
	return r.String()
}

// normalizeBaseURL normalizes a base URL string
func normalizeBaseURL(urlStr string) string {
	normalized := strings.TrimSpace(urlStr)

	// If empty, return default
	if normalized == "" {
		return "https://app.pangolin.net"
	}

	// Add https:// if no scheme
	if !strings.HasPrefix(normalized, "http://") && !strings.HasPrefix(normalized, "https://") {
		normalized = "https://" + normalized
	}

	// Remove trailing slashes
	normalized = strings.TrimRight(normalized, "/")

	return normalized
}

// apiURL constructs the full API URL for a given path
func (c *APIClient) apiURL(path string) (string, error) {
	return apiURLFor(c.CurrentBaseURL(), path)
}

func apiURLFor(baseURL, path string) (string, error) {
	// Ensure path starts with /
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}

	// Add /api/v1 prefix
	apiPath := "/api/v1" + path

	// Construct full URL
	fullURL := baseURL + apiPath

	// Validate URL
	_, err := url.Parse(fullURL)
	if err != nil {
		logger.Error("Error: Invalid URL constructed: %s (baseURL: %s, path: %s)", fullURL, baseURL, path)
		return "", &APIError{Type: ErrorTypeInvalidURL}
	}

	return fullURL, nil
}

// makeRequest makes an HTTP request and returns the response data and status
func (c *APIClient) makeRequest(method, path string, body []byte) ([]byte, *http.Response, error) {
	return c.doRequest(context.Background(), method, path, body, true)
}

// makeRequestContext is makeRequest, aborted when ctx is done.
func (c *APIClient) makeRequestContext(ctx context.Context, method, path string, body []byte) ([]byte, *http.Response, error) {
	return c.doRequest(ctx, method, path, body, true)
}

// doRequest sends one request built from a single snapshot of the client's
// server and session. notifyUnauthorized controls whether a 401 reports the
// session as expired.
func (c *APIClient) doRequest(ctx context.Context, method, path string, body []byte, notifyUnauthorized bool) ([]byte, *http.Response, error) {
	sess := c.snapshot()
	fullURL, err := apiURLFor(sess.baseURL, path)
	if err != nil {
		return nil, nil, err
	}

	var bodyReader io.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	}

	req, err := http.NewRequestWithContext(ctx, method, fullURL, bodyReader)
	if err != nil {
		return nil, nil, &APIError{Type: ErrorTypeInvalidURL, Err: err}
	}

	// Set headers
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", version.UserAgent())
	req.Header.Set("X-CSRF-Token", c.csrfToken)

	// Add session cookie if available
	if sess.token != "" {
		req.Header.Set("Cookie", fmt.Sprintf("%s=%s", sess.cookieName, sess.token))
	}

	logger.Debug("Making request to: %s", fullURL)

	resp, err := c.client.Do(req)
	if err != nil {
		// A cancelled request is not a network problem; callers check for it.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, nil, &APIError{Type: ErrorTypeNetworkError, Message: ctxErr.Error(), Err: ctxErr}
		}
		// Handle network errors with more specific messages
		if urlErr, ok := err.(*url.Error); ok {
			if urlErr.Timeout() {
				msg := fmt.Sprintf("Connection to %s timed out. Please check your network connection.", sess.baseURL)
				logger.Error("%s", msg)
				return nil, nil, &APIError{Type: ErrorTypeHTTPError, Message: msg, Err: err}
			}
			if urlErr.Temporary() {
				msg := fmt.Sprintf("Temporary network error connecting to %s: %v", sess.baseURL, urlErr)
				logger.Error("%s", msg)
				return nil, nil, &APIError{Type: ErrorTypeNetworkError, Message: msg, Err: err}
			}
		}

		// Generic network error
		logger.Error("Network error: %v", err)
		return nil, nil, &APIError{Type: ErrorTypeNetworkError, Err: err}
	}
	defer resp.Body.Close()

	// Read response body
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		logger.Error("Error reading response body: %v", err)
		return nil, resp, &APIError{Type: ErrorTypeInvalidResponse, Err: err}
	}

	// Notify when an authenticated request gets 401 so session-expired state can be set.
	// 403 means the token is accepted but this action is forbidden — leave the session alone.
	// Only report it while the client still uses the token the request was
	// sent with: a request from before an account switch or logout must not
	// mark the session that replaced it as expired.
	if resp.StatusCode == 401 && notifyUnauthorized && sess.token != "" {
		c.mu.RLock()
		fn := c.onUnauthorized
		current := c.sessionToken == sess.token && c.baseURL == sess.baseURL
		c.mu.RUnlock()
		if fn != nil && current {
			fn(sess.token)
		}
	}

	return data, resp, nil
}

// parseResponse parses the API response and returns the data
func (c *APIClient) parseResponse(data []byte, resp *http.Response, result interface{}) error {
	// Check HTTP status first
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Try to parse error message from response
		var errorResponse APIResponse[EmptyResponse]
		if err := json.Unmarshal(data, &errorResponse); err == nil {
			message := errorResponse.Message
			if message == "" {
				message = getDefaultHTTPErrorMessage(resp.StatusCode)
			}
			return &APIError{
				Type:    ErrorTypeHTTPError,
				Status:  resp.StatusCode,
				Message: message,
			}
		}

		// Fallback to default error message
		return &APIError{
			Type:    ErrorTypeHTTPError,
			Status:  resp.StatusCode,
			Message: getDefaultHTTPErrorMessage(resp.StatusCode),
		}
	}

	// Handle empty responses
	if len(data) == 0 || (len(data) == 2 && string(data) == "{}") {
		// Check if result is EmptyResponse
		if emptyResp, ok := result.(*EmptyResponse); ok {
			*emptyResp = EmptyResponse{}
			return nil
		}
		return &APIError{Type: ErrorTypeInvalidResponse, Message: "Empty response"}
	}

	// Parse API response wrapper
	var apiResponse APIResponse[json.RawMessage]
	if err := json.Unmarshal(data, &apiResponse); err != nil {
		logger.Error("Error parsing API response: %v", err)
		return &APIError{Type: ErrorTypeDecodingError, Err: err}
	}

	// Check API-level success/error flags
	if apiResponse.Success != nil && !*apiResponse.Success {
		message := apiResponse.Message
		if message == "" {
			message = "Request failed"
		}
		status := apiResponse.Status
		if status == 0 {
			status = resp.StatusCode
		}
		return &APIError{
			Type:    ErrorTypeHTTPError,
			Status:  status,
			Message: message,
		}
	}

	if apiResponse.Error != nil && *apiResponse.Error {
		message := apiResponse.Message
		if message == "" {
			message = "Request failed"
		}
		status := apiResponse.Status
		if status == 0 {
			status = resp.StatusCode
		}
		return &APIError{
			Type:    ErrorTypeHTTPError,
			Status:  status,
			Message: message,
		}
	}

	// Extract data from response
	if len(apiResponse.Data) > 0 {
		if err := json.Unmarshal(apiResponse.Data, result); err != nil {
			logger.Error("Error decoding response data: %v", err)
			return &APIError{Type: ErrorTypeDecodingError, Err: err}
		}
	} else {
		// If data is nil but response was successful, try to return EmptyResponse
		if emptyResp, ok := result.(*EmptyResponse); ok {
			*emptyResp = EmptyResponse{}
			return nil
		}
		return &APIError{Type: ErrorTypeInvalidResponse, Message: "No data in response"}
	}

	return nil
}

// getDefaultHTTPErrorMessage returns a default error message for HTTP status codes
func getDefaultHTTPErrorMessage(statusCode int) string {
	switch statusCode {
	case 401, 403:
		return "Unauthorized"
	case 404:
		return "Not found"
	case 429:
		return "Rate limit exceeded"
	case 500:
		return "Internal server error"
	default:
		return fmt.Sprintf("HTTP error %d", statusCode)
	}
}

// extractCookie extracts a cookie value from HTTP response headers
func extractCookie(resp *http.Response, name string) string {
	// Check Set-Cookie headers
	for _, cookie := range resp.Cookies() {
		if cookie.Name == name {
			return cookie.Value
		}
	}

	// Also check Set-Cookie header directly (for cases where cookies aren't parsed)
	setCookie := resp.Header.Get("Set-Cookie")
	if setCookie != "" {
		// Parse cookie string (format: "name=value; Path=/; ...")
		parts := strings.Split(setCookie, ";")
		for _, part := range parts {
			part = strings.TrimSpace(part)
			if strings.HasPrefix(part, name+"=") {
				value := strings.TrimPrefix(part, name+"=")
				return value
			}
		}
	}

	// Check for multiple Set-Cookie headers
	for key, values := range resp.Header {
		if strings.ToLower(key) == "set-cookie" {
			for _, value := range values {
				parts := strings.Split(value, ";")
				for _, part := range parts {
					part = strings.TrimSpace(part)
					if strings.HasPrefix(part, name+"=") {
						cookieValue := strings.TrimPrefix(part, name+"=")
						return cookieValue
					}
				}
			}
		}
	}

	return ""
}

// HTTPStatus returns the HTTP status of an API error, or 0 when err is not an
// HTTP error response (no connection, a timeout, a cancelled request).
func HTTPStatus(err error) int {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.Status
	}
	return 0
}

// IsSessionRejected reports whether the server refused the session (401/403).
func IsSessionRejected(err error) bool {
	status := HTTPStatus(err)
	return status == http.StatusUnauthorized || status == http.StatusForbidden
}

// IsTransient reports whether err may pass by itself and says nothing about
// the session: no connection, a timeout, rate limiting (429), a server error
// (5xx), or a response that could not be read or decoded.
func IsTransient(err error) bool {
	if err == nil {
		return false
	}
	status := HTTPStatus(err)
	return status == 0 || status == http.StatusTooManyRequests || status >= 500
}
