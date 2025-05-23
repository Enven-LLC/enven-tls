package tls_client

import (
	"fmt"
	"io"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/Enven-LLC/enven-tls/profiles"

	http "github.com/bogdanfinn/fhttp"
	"golang.org/x/net/proxy"
)

var defaultRedirectFunc = func(req *http.Request, via []*http.Request) error {
	return http.ErrUseLastResponse
}

type HttpClient interface {
	GetCookies(u *url.URL) []*http.Cookie
	SetCookies(u *url.URL, cookies []*http.Cookie)
	SetCookieJar(jar http.CookieJar)
	GetCookieJar() http.CookieJar

	SetBJar(jar *BetterJar)
	GetBJar() *BetterJar

	SetProxy(proxyUrl string) error
	GetProxy() string
	SetFollowRedirect(followRedirect bool)
	GetFollowRedirect() bool
	CloseIdleConnections()
	Do(req *http.Request) (*WebResp, error)
}

// Interface guards are a cheap way to make sure all methods are implemented, this is a static check and does not affect runtime performance.
var _ HttpClient = (*httpClient)(nil)

type httpClient struct {
	bjar *BetterJar
	http.Client
	headerLck sync.Mutex
	logger    Logger
	config    *httpClientConfig
}

var DefaultTimeoutSeconds = 30

var DefaultOptions = []HttpClientOption{
	WithTimeoutSeconds(DefaultTimeoutSeconds),
	WithClientProfile(profiles.DefaultClientProfile),
	WithRandomTLSExtensionOrder(),
	WithNotFollowRedirects(),
}

func ProvideDefaultClient(logger Logger) (HttpClient, error) {
	jar := NewCookieJar()

	return NewHttpClient(logger, append(DefaultOptions, WithCookieJar(jar))...)
}

// NewHttpClient constructs a new HTTP client with the given logger and client options.
func NewHttpClient(logger Logger, options ...HttpClientOption) (HttpClient, error) {
	config := &httpClientConfig{
		followRedirects:    true,
		badPinHandler:      nil,
		customRedirectFunc: nil,
		connectHeaders:     make(http.Header),
		defaultHeaders:     make(http.Header),
		clientProfile:      profiles.DefaultClientProfile,
		timeout:            time.Duration(DefaultTimeoutSeconds) * time.Second,
	}

	for _, opt := range options {
		opt(config)
	}

	if err := validateConfig(config); err != nil {
		return nil, err
	}

	client, clientProfile, err := buildFromConfig(logger, config)
	if err != nil {
		return nil, err
	}

	config.clientProfile = clientProfile

	if config.debug {
		if logger == nil {
			logger = NewLogger()
		}

		logger = NewDebugLogger(logger)
	}

	if logger == nil {
		logger = NewNoopLogger()
	}

	c := &httpClient{
		Client:    *client,
		logger:    logger,
		config:    config,
		headerLck: sync.Mutex{},
	}
	if config.betterJar != nil {
		c.bjar = config.betterJar
	}

	return c, nil
}

func validateConfig(_ *httpClientConfig) error {
	return nil
}

func buildFromConfig(logger Logger, config *httpClientConfig) (*http.Client, profiles.ClientProfile, error) {
	dialer := newDirectDialer(config.timeout, config.localAddr, config.dialer)

	if config.proxyUrl != "" {
		proxyDialer, err := newConnectDialer(config.proxyUrl, config.timeout, config.localAddr, config.dialer, config.connectHeaders, logger)
		if err != nil {
			return nil, profiles.ClientProfile{}, err
		}
		dialer = proxyDialer
	}

	var redirectFunc func(req *http.Request, via []*http.Request) error
	if !config.followRedirects {
		redirectFunc = defaultRedirectFunc
	} else {
		redirectFunc = nil

		if config.customRedirectFunc != nil {
			redirectFunc = config.customRedirectFunc
		}
	}

	clientProfile := config.clientProfile

	transport, err := newRoundTripper(clientProfile, config.transportOptions, config.serverNameOverwrite, config.insecureSkipVerify, config.withRandomTlsExtensionOrder, config.forceHttp1, config.certificatePins, config.badPinHandler, config.disableIPV6, config.disableIPV4, dialer)
	if err != nil {
		return nil, clientProfile, err
	}

	client := &http.Client{
		Timeout:       config.timeout,
		Transport:     transport,
		CheckRedirect: redirectFunc,
	}

	if config.cookieJar != nil {
		client.Jar = config.cookieJar
	}

	return client, clientProfile, nil
}

// CloseIdleConnections closes all idle connections of the underlying http client.
func (c *httpClient) CloseIdleConnections() {
	c.Client.CloseIdleConnections()
}

// SetFollowRedirect configures the client's HTTP redirect following policy.
func (c *httpClient) SetFollowRedirect(followRedirect bool) {
	c.logger.Debug("set follow redirect from %v to %v", c.config.followRedirects, followRedirect)

	c.config.followRedirects = followRedirect
	c.applyFollowRedirect()
}

// GetFollowRedirect returns the client's HTTP redirect following policy.
func (c *httpClient) GetFollowRedirect() bool {
	return c.config.followRedirects
}

func (c *httpClient) applyFollowRedirect() {
	if c.config.followRedirects {
		c.logger.Debug("automatic redirect following is enabled")
		c.CheckRedirect = nil
	} else {
		c.logger.Debug("automatic redirect following is disabled")
		c.CheckRedirect = defaultRedirectFunc
	}

	if c.config.customRedirectFunc != nil && c.config.followRedirects {
		c.CheckRedirect = c.config.customRedirectFunc
	}
}

// SetProxy configures the client to use the given proxy URL.
//
// proxyUrl should be formatted as:
//
//	"http://user:pass@host:port"
func (c *httpClient) SetProxy(proxyUrl string) error {
	currentProxy := c.config.proxyUrl

	c.logger.Debug("set proxy from %s to %s", c.config.proxyUrl, proxyUrl)
	c.config.proxyUrl = proxyUrl

	err := c.applyProxy()
	if err != nil {
		c.logger.Error("failed to apply new proxy. rolling back to previous used proxy: %w", err)
		c.config.proxyUrl = currentProxy

		return c.applyProxy()
	}

	return nil
}

// GetProxy returns the proxy URL used by the client.
func (c *httpClient) GetProxy() string {
	return c.config.proxyUrl
}

func (c *httpClient) applyProxy() error {
	var dialer proxy.ContextDialer
	dialer = proxy.Direct

	if c.config.proxyUrl != "" {
		c.logger.Debug("proxy url %s supplied - using proxy connect dialer", c.config.proxyUrl)
		proxyDialer, err := newConnectDialer(c.config.proxyUrl, c.config.timeout, c.config.localAddr, c.config.dialer, c.config.connectHeaders, c.logger)
		if err != nil {
			c.logger.Error("failed to create proxy connect dialer: %s", err.Error())
			return err
		}

		dialer = proxyDialer
	}

	transport, err := newRoundTripper(c.config.clientProfile, c.config.transportOptions, c.config.serverNameOverwrite, c.config.insecureSkipVerify, c.config.withRandomTlsExtensionOrder, c.config.forceHttp1, c.config.certificatePins, c.config.badPinHandler, c.config.disableIPV6, c.config.disableIPV4, dialer)
	if err != nil {
		return err
	}

	c.Transport = transport

	return nil
}

// GetCookies returns the cookies in the client's cookie jar for a given URL.
func (c *httpClient) GetCookies(u *url.URL) []*http.Cookie {
	c.logger.Debug(fmt.Sprintf("get cookies for url: %s", u.String()))
	if c.Jar == nil {
		c.logger.Warn("you did not setup a cookie jar")
		return nil
	}

	return c.Jar.Cookies(u)
}

// SetCookies sets a list of cookies for a given URL in the client's cookie jar.
func (c *httpClient) SetCookies(u *url.URL, cookies []*http.Cookie) {
	c.logger.Debug(fmt.Sprintf("set cookies for url: %s", u.String()))

	if c.Jar == nil {
		c.logger.Warn("you did not setup a cookie jar")
		return
	}

	c.Jar.SetCookies(u, cookies)
}

func (c *httpClient) SetBJar(jar *BetterJar) {
	c.bjar = jar
}
func (c *httpClient) GetBJar() *BetterJar {
	return c.bjar
}

// SetCookieJar sets a jar as the clients cookie jar. This is the recommended way when you want to "clear" the existing cookiejar
func (c *httpClient) SetCookieJar(jar http.CookieJar) {
	c.Jar = jar
}

// GetCookieJar returns the jar the client is currently using
func (c *httpClient) GetCookieJar() http.CookieJar {
	return c.Jar
}

// Do issues a given HTTP request and returns the corresponding response.
//
// If the returned error is nil, the response contains a non-nil body, which the user is expected to close.
func (c *httpClient) Do(req *http.Request) (*WebResp, error) {

	if c.bjar != nil {
		cookies := c.bjar.GetCookies()
		if cookies != "" {
			req.Header.Set("Cookie", cookies)
		}
	}

	// Header order must be defined in all lowercase. On HTTP 1 people sometimes define them also in uppercase and then ordering does not work.
	c.headerLck.Lock()

	if len(req.Header) == 0 {
		req.Header = c.config.defaultHeaders.Clone()
	}

	req.Header[http.HeaderOrderKey] = allToLower(req.Header[http.HeaderOrderKey])
	c.headerLck.Unlock()

	resp, err := c.Client.Do(req)
	if err != nil {
		return &WebResp{StatusCode: -1}, err
	}

	webResp := &WebResp{
		Status:           resp.Status,
		StatusCode:       resp.StatusCode,
		Proto:            resp.Proto,
		ProtoMajor:       resp.ProtoMajor,
		ProtoMinor:       resp.ProtoMinor,
		Header:           resp.Header,
		ContentLength:    resp.ContentLength,
		TransferEncoding: resp.TransferEncoding,
		Close:            resp.Close,
		Uncompressed:     resp.Uncompressed,
		Trailer:          resp.Trailer,
		Request:          resp.Request,
		TLS:              resp.TLS,
	}

	if c.bjar != nil {
		c.bjar.processCookies(webResp)
	}
	//todo: process regular jar

	defer resp.Body.Close()

	// read body
	webResp.BodyBytes, err = io.ReadAll(resp.Body)
	if err != nil {
		return webResp, err
	}

	webResp.Body = string(webResp.BodyBytes)
	return webResp, nil
}

func allToLower(list []string) []string {
	lower := make([]string, len(list))

	for i, elem := range list {
		lower[i] = strings.ToLower(elem)
	}

	return lower
}
