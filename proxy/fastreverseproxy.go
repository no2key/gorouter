package proxy

import (
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"code.cloudfoundry.org/gorouter/access_log/schema"
	router_http "code.cloudfoundry.org/gorouter/common/http"
	"code.cloudfoundry.org/gorouter/metrics/reporter"
	"code.cloudfoundry.org/gorouter/proxy/handler"
	"code.cloudfoundry.org/gorouter/proxy/utils"
	"code.cloudfoundry.org/gorouter/route"
	"code.cloudfoundry.org/gorouter/routeservice"
	"code.cloudfoundry.org/lager"
)

// HopHeaders are hop-by-hop headers that are removed when sent to the backend.
// http://www.w3.org/Protocols/rfc2616/rfc2616-sec13.html
var HopHeaders = []string{
	"Connection",
	"Proxy-Connection", // non-standard but still sent by libcurl and rejected by e.g. google
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Te",      // canonicalized version of "TE"
	"Trailer", // not Trailers per URL above; http://www.rfc-editor.org/errata_search.php?eid=4522
	"Transfer-Encoding",
	"Upgrade",
}

var xForwardedForKey = "X-Forwarded-For"

const (
	maxRetries = 3
)

// FastReverseProxy is responsible for proxying requests to the backend using
// fasthttp
type FastReverseProxy struct {
	registry                 LookupRegistry
	logger                   lager.Logger
	reporter                 reporter.ProxyReporter
	routeServiceConfig       *routeservice.RouteServiceConfig
	defaultLoadBalance       string
	forceForwardedProtoHttps bool
	traceKey                 string
	ip                       string
	secureCookies            bool
	tlsConfig                *tls.Config
	endpointTimeout          time.Duration
}

// NewFastReverseProxy creates a new FastReverseProxy
func NewFastReverseProxy(registry LookupRegistry, logger lager.Logger,
	reporter reporter.ProxyReporter, routeServiceConfig *routeservice.RouteServiceConfig,
	forceForwardedProtoHttps bool,
	traceKey string, defaultLoadBalance string,
	ip string, secureCookies bool, tlsConfig *tls.Config, endpointTimeout time.Duration) *FastReverseProxy {

	return &FastReverseProxy{
		registry:                 registry,
		logger:                   logger,
		reporter:                 reporter,
		forceForwardedProtoHttps: forceForwardedProtoHttps,
		routeServiceConfig:       routeServiceConfig,
		traceKey:                 traceKey,
		ip:                       ip,
		defaultLoadBalance:       defaultLoadBalance,
		tlsConfig:                tlsConfig,
		endpointTimeout:          endpointTimeout,
		//		secureCookies:            secureCookies,
	}
}

func (f *FastReverseProxy) ServeHTTP(rw http.ResponseWriter, req *http.Request, next http.HandlerFunc) {
	proxyWriter := rw.(utils.ProxyResponseWriter)
	alr := proxyWriter.Context().Value("AccessLogRecord")
	if alr == nil {
		fmt.Println("AccessLogRecord not set on context", errors.New("failed-to-access-LogRecord"))
	}
	accessLog := alr.(*schema.AccessLogRecord)

	requestHandler := handler.AcquireHandler()
	defer handler.ReleaseHandler(requestHandler)
	requestHandler.UpdateRequestHandler(req, proxyWriter, f.reporter, accessLog, f.logger)

	var closer io.ReadCloser
	if req.Body != nil {
		closer = req.Body
		req.Body = ioutil.NopCloser(req.Body)
		defer closer.Close()
	}

	backendReq := req

	for _, h := range HopHeaders {
		backendReq.Header.Del(h)
	}

	if clientIP, _, err := net.SplitHostPort(req.RemoteAddr); err == nil {
		// If we aren't the first proxy retain prior
		// X-Forwarded-For information as a comma+space
		// separated list and fold multiple headers into one.
		var clientIPKey string
		clientIPKey = clientIP
		prior := backendReq.Header.Get(xForwardedForKey)
		if prior != "" {
			clientIPKey = fmt.Sprintf("%s, %s", prior, clientIP)
		}
		backendReq.Header.Set(xForwardedForKey, clientIPKey)
	}

	if !isProtocolSupported(req) {
		requestHandler.HandleUnsupportedProtocol()
		return
	}

	if f.forceForwardedProtoHttps {
		backendReq.Header.Set("X-Forwarded-Proto", "https")
	} else if req.Header.Get("X-Forwarded-Proto") == "" {
		scheme := "http"
		if req.TLS != nil {
			scheme = "https"
		}
		backendReq.Header.Set("X-Forwarded-Proto", scheme)
	}

	requestPath := req.URL.EscapedPath()
	uri := route.Uri(hostWithoutPort(req) + requestPath)
	pool := f.registry.Lookup(uri)
	if pool == nil {
		requestHandler.HandleMissingRoute()
		return
	}

	stickyEndpointId := getStickySession(req)
	iter := &wrappedIterator{
		nested: pool.Endpoints(f.defaultLoadBalance, stickyEndpointId),

		afterNext: func(endpoint *route.Endpoint) {
			if endpoint != nil {
				accessLog.RouteEndpoint = endpoint
				f.reporter.CaptureRoutingRequest(endpoint, req)
			}
		},
	}

	if isTcpUpgrade(req) {
		requestHandler.HandleTcpRequest(iter)
		return
	}

	if isWebSocketUpgrade(req) {
		requestHandler.HandleWebSocketRequest(iter)
		return
	}

	backend := true
	routeServiceUrl := pool.RouteServiceUrl()
	// Attempted to use a route service when it is not supported
	if routeServiceUrl != "" && !f.routeServiceConfig.RouteServiceEnabled() {
		requestHandler.HandleUnsupportedRouteService()
		return
	}

	var routeServiceArgs routeservice.RouteServiceRequest
	if routeServiceUrl != "" {
		rsSignature := req.Header.Get(routeservice.RouteServiceSignature)

		var recommendedScheme string

		if f.routeServiceConfig.RouteServiceRecommendHttps() {
			recommendedScheme = "https"
		} else {
			recommendedScheme = "http"
		}

		forwardedUrlRaw := recommendedScheme + "://" + hostWithoutPort(req) + req.RequestURI
		if hasBeenToRouteService(routeServiceUrl, rsSignature) {
			// A request from a route service destined for a backend instances
			routeServiceArgs.URLString = routeServiceUrl
			err := f.routeServiceConfig.ValidateSignature(&req.Header, forwardedUrlRaw)
			if err != nil {
				requestHandler.HandleBadSignature(err)
				return
			}
			backendReq.Header.Del(routeservice.RouteServiceSignature)
			backendReq.Header.Del(routeservice.RouteServiceMetadata)
			backendReq.Header.Del(routeservice.RouteServiceForwardedURL)
		} else {
			var err error
			// should not hardcode http, will be addressed by #100982038
			routeServiceArgs, err = f.routeServiceConfig.Request(routeServiceUrl, forwardedUrlRaw)
			backend = false
			if err != nil {
				requestHandler.HandleRouteServiceFailure(err)
				return
			}
			backendReq.Header.Set(routeservice.RouteServiceSignature, routeServiceArgs.Signature)
			backendReq.Header.Set(routeservice.RouteServiceMetadata, routeServiceArgs.Metadata)
			backendReq.Header.Set(routeservice.RouteServiceForwardedURL, routeServiceArgs.ForwardedURL)
		}
	}

	var backendResp *http.Response
	var endpoint *route.Endpoint
	var err error
	for retry := 0; retry < maxRetries; retry++ {
		endpoint, err = selectEndpoint(iter)

		if err != nil {
			break
		}
		setupRequest(backendReq, endpoint)

		iter.PreRequest(endpoint)
		//		var hc fasthttp.HostClient
		var hc http.Client
		var netTransport = &http.Transport{
			Dial: (&net.Dialer{
				Timeout: 5 * time.Second,
			}).Dial,
			TLSHandshakeTimeout: 5 * time.Second,
			DisableKeepAlives:   true,
		}
		hc.Transport = netTransport
		setupProxyRequest(req, backendReq, false)
		backendReq.RequestURI = ""

		if backend {
			backendReq.URL.Host = endpoint.CanonicalAddr()
		}
		backendResp, err = hc.Do(backendReq)

		iter.PostRequest(endpoint)
		if err != nil {
			fmt.Println("HTTP Error:", err.Error())
		}
		if err == nil {
			break
		}
		if !retryableError(err) {
			break
		}

		// TODO: Log error timed out connecting to backends
	}

	if err != nil {
		requestHandler.HandleBadGateway(err, req)
		rw.Write([]byte("Exceeded max retries: Timed out connecting to backends."))
		return
	}

	if backendResp != nil {
		accessLog.StatusCode = backendResp.StatusCode
	}

	if f.traceKey != "" && endpoint != nil && req.Header.Get(router_http.VcapTraceHeader) == f.traceKey {
		router_http.SetTraceHeaders(rw, f.ip, endpoint.CanonicalAddr())
	}

	// TODO: add trailers?
	for _, h := range HopHeaders {
		backendResp.Header.Del(h)
	}
	h := backendResp.Header

	for k, v := range h {
		rw.Header().Set(k, strings.Join(v, ","))
	}
	// if Content-Type not in response, nil out to suppress Go's auto-detect
	if contentType := backendResp.Header.Get("Content-Type"); len(contentType) == 0 {
		rw.Header()["Content-Type"] = nil
	}

	if len(backendResp.Trailer) > 0 {
		trailerKeys := make([]string, 0, len(backendResp.Trailer))
		for k := range backendResp.Trailer {
			trailerKeys = append(trailerKeys, k)
		}
		rw.Header().Add("Trailer", strings.Join(trailerKeys, ", "))
	}

	rw.WriteHeader(backendResp.StatusCode)

	if len(backendResp.Trailer) > 0 {
		// Force chunking if we saw a response trailer.
		// This prevents net/http from calculating the length for short
		// bodies and adding a Content-Length.
		if fl, ok := rw.(http.Flusher); ok {
			fl.Flush()
		}
	}

	f.copyResponse(rw, backendResp.Body)
	backendResp.Body.Close()

	for k, vv := range backendResp.Trailer {
		for _, v := range vv {
			rw.Header().Add(k, v)
		}
	}

	next(rw, req)

}

// why are we failing to convert the obj
func retryableError(err error) bool {
	netErrString := err.Error()
	return strings.Contains(netErrString, "dial")
}

//Until onExitFlushLoop the following is copied from golang release-candidate 1.7 reverse_proxy.go
func (p *FastReverseProxy) copyResponse(dst io.Writer, src io.Reader) {
	if wf, ok := dst.(writeFlusher); ok {
		mlw := &maxLatencyWriter{
			dst:     wf,
			latency: 50 * time.Millisecond,
			done:    make(chan bool),
		}
		go mlw.flushLoop()
		defer mlw.stop()
		dst = mlw
	}

	var buf []byte
	_, err := io.CopyBuffer(dst, src, buf)
	if err != nil {
	}
}

type writeFlusher interface {
	io.Writer
	http.Flusher
}

type maxLatencyWriter struct {
	dst     writeFlusher
	latency time.Duration

	mu   sync.Mutex // protects Write + Flush
	done chan bool
}

func (m *maxLatencyWriter) Write(p []byte) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.dst.Write(p)
}

func (m *maxLatencyWriter) flushLoop() {
	t := time.NewTicker(m.latency)
	defer t.Stop()
	for {
		select {
		case <-m.done:
			if onExitFlushLoop != nil {
				onExitFlushLoop()
			}
			return
		case <-t.C:
			m.mu.Lock()
			m.dst.Flush()
			m.mu.Unlock()
		}
	}
}

func (m *maxLatencyWriter) stop() { m.done <- true }

// onExitFlushLoop is a callback set by tests to detect the state of the
// flushLoop() goroutine.
var onExitFlushLoop func()

// func setupStickySession(responseWriter http.ResponseWriter, backendRespHeaders *fasthttp.ResponseHeader,
// 	endpoint *route.Endpoint,
// 	originalEndpointId string,
// 	secureCookies bool,
// 	path string) {
// 	secure := false
// 	maxAge := 0

// 	// did the endpoint change?
// 	sticky := originalEndpointId != "" && originalEndpointId != endpoint.PrivateInstanceId

// 	cookieFunc := func(key, value []byte) {
// 		if string(key) == StickyCookieKey {
// 			sticky = true
// 			// TODO: parse resp cookie to get the max age since fhttp does not support this feature
// 			//	if v.MaxAge < 0 {
// 			//			maxAge = v.MaxAge
// 			//		}
// 			//		secure = v.Secure
// 			//			break
// 		}
// 	}

// 	backendRespHeaders.VisitAllCookie(cookieFunc)
// 	if sticky {
// 		// right now secure attribute would as equal to the JSESSION ID cookie (if present),
// 		// but override if set to true in config
// 		if secureCookies {
// 			secure = true
// 		}

// 		cookie := &http.Cookie{
// 			Name:     VcapCookieId,
// 			Value:    endpoint.PrivateInstanceId,
// 			Path:     path,
// 			MaxAge:   maxAge,
// 			HttpOnly: true,
// 			Secure:   secure,
// 		}

// 		http.SetCookie(responseWriter, cookie)
// 	}
// }

// func copyRequest(req *http.Request, newReq *http.Request) (io.ReadCloser, error) {
// 	fmt.Println(req.TransferEncoding)
// 	var closer io.ReadCloser
// 	if req.Body != nil {
// 		closer = req.Body
// 		req.Body = ioutil.NopCloser(req.Body)
// 	}
// 	if len(req.TransferEncoding) > 0 && req.TransferEncoding[0] == "chunked" {
// 		fmt.Println("CHUNKED REQUEST INCOMING")

// 		buf := new(bytes.Buffer)
// 		fmt.Println("Reading request...")
// 		err := req.Write(buf)
// 		if err != nil {
// 			return nil, err
// 		}

// 		fmt.Println("Finished reading request", buf.String())

// 		newReq.Header.SetMethod(req.Method)
// 		newReq.Header.SetHost(req.Host)
// 		for k, v := range req.Header {
// 			for _, hv := range v {
// 				newReq.Header.Add(k, hv)
// 			}
// 		}
// 		if req.RequestURI != "" {
// 			newReq.SetRequestURI(req.RequestURI)
// 		} else {
// 			newReq.SetRequestURI(req.URL.RequestURI())
// 		}

// 		newReq.SetBodyStream(req.Body, -1)
// 	} else {
// 		buf := new(bytes.Buffer)
// 		fmt.Println("Reading request...")
// 		err := req.Write(buf)
// 		if err != nil {
// 			return nil, err
// 		}

// 		fmt.Println("Finished reading request", buf.String())

// 		err = newReq.Read(bufio.NewReader(buf))
// 		if err != nil {
// 			return nil, err
// 		}
// 		if req.RequestURI != "" {
// 			newReq.SetRequestURI(req.RequestURI)
// 		}
// 		fmt.Println("Finished copying request", newReq.String())

// 		// newBuf := new(bytes.Buffer)
// 		// writer := bufio.NewWriter(newBuf)
// 		// newReq.Write(writer)
// 		// writer.Flush()
// 		//
// 		// fmt.Println("copied req", newBuf.String())
// 	}

// 	return closer, nil
// }

func setupRequest(request *http.Request, endpoint *route.Endpoint) {
	request.Header.Set("X-CF-ApplicationID", endpoint.ApplicationId) // why do we need this ?
	value := endpoint.PrivateInstanceId
	if value == "" {
		value = endpoint.CanonicalAddr()
	}

	request.Header.Set(router_http.CfInstanceIdHeader, value)
	// if ok := request.Header.Peek(http.CanonicalHeaderKey("X-Request-Start")); string(ok) != "" {
	// 	request.Header.Set("X-Request-Start", strconv.FormatInt(time.Now().UnixNano()/1e6, 10))
	// }
}

func hostWithoutPort(req *http.Request) string {
	host := req.Host

	// Remove :<port>
	pos := strings.Index(host, ":")
	if pos >= 0 {
		host = host[0:pos]
	}

	return host
}

func selectEndpoint(iter *wrappedIterator) (*route.Endpoint, error) {
	endpoint := iter.Next()
	if endpoint == nil {
		return nil, handler.NoEndpointsAvailable
	}

	//	rt.logger = rt.logger.WithData(lager.Data{"route-endpoint": endpoint.ToLogData()})
	return endpoint, nil
}

func isProtocolSupported(request *http.Request) bool {
	return request.ProtoMajor == 1 && (request.ProtoMinor == 0 || request.ProtoMinor == 1)
}

func getStickySession(request *http.Request) string {
	// Try choosing a backend using sticky session
	if _, err := request.Cookie(StickyCookieKey); err == nil {
		if sticky, err := request.Cookie(VcapCookieId); err == nil {
			return sticky.Value
		}
	}
	return ""
}

func isWebSocketUpgrade(request *http.Request) bool {
	// websocket should be case insensitive per RFC6455 4.2.1
	return strings.ToLower(upgradeHeader(request)) == "websocket"
}

func isTcpUpgrade(request *http.Request) bool {
	return upgradeHeader(request) == "tcp"
}
