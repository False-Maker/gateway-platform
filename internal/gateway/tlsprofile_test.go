package gateway

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/elucid/gateway-platform/internal/events"
	"github.com/elucid/gateway-platform/pkg/contracts"
)

// helloRecorder is a TLS server that records the ClientHello it received.
type helloRecorder struct {
	mu     sync.Mutex
	hellos []*tls.ClientHelloInfo
}

func (r *helloRecorder) record(info *tls.ClientHelloInfo) (*tls.Config, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	copied := *info
	r.hellos = append(r.hellos, &copied)
	return nil, nil
}

func (r *helloRecorder) last(t *testing.T) *tls.ClientHelloInfo {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.hellos) == 0 {
		t.Fatal("no ClientHello recorded")
	}
	return r.hellos[len(r.hellos)-1]
}

func newRecordingTLSServer(t *testing.T, recorder *helloRecorder, handler http.Handler) (*httptest.Server, *x509.CertPool) {
	t.Helper()
	server := httptest.NewUnstartedServer(handler)
	server.TLS = &tls.Config{GetConfigForClient: recorder.record, NextProtos: []string{"http/1.1"}}
	server.StartTLS()
	pool := x509.NewCertPool()
	pool.AddCert(server.Certificate())
	return server, pool
}

func fingerprintedClient(t *testing.T, pool *x509.CertPool, profile string) *http.Client {
	t.Helper()
	base := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}, Timeout: 5 * time.Second}
	client, err := clientForTLSProfile(base, profile)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestCodexRustlsProfileShapesClientHello(t *testing.T) {
	recorder := &helloRecorder{}
	server, pool := newRecordingTLSServer(t, recorder, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") }))
	defer server.Close()
	response, err := fingerprintedClient(t, pool, "codex_rustls").Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	hello := recorder.last(t)
	spec := codexRustlsClientHelloSpec("")
	if len(hello.CipherSuites) != len(spec.CipherSuites) {
		t.Fatalf("cipher suites=%d want %d", len(hello.CipherSuites), len(spec.CipherSuites))
	}
	for i := range spec.CipherSuites {
		if hello.CipherSuites[i] != spec.CipherSuites[i] {
			t.Fatalf("cipher suite order differs at %d: %#x want %#x", i, hello.CipherSuites[i], spec.CipherSuites[i])
		}
	}
	// rustls hello: first three curves x25519, P-256, then 0x001e (x448), no ALPN, 20 signature schemes.
	if len(hello.SupportedCurves) != 10 || hello.SupportedCurves[0] != tls.X25519 || hello.SupportedCurves[1] != tls.CurveP256 || hello.SupportedCurves[2] != tls.CurveID(0x001e) {
		t.Fatalf("supported curves=%v", hello.SupportedCurves)
	}
	if len(hello.SupportedProtos) != 0 {
		t.Fatalf("rustls profile must not send ALPN, got %v", hello.SupportedProtos)
	}
	if len(hello.SignatureSchemes) != 20 || hello.SignatureSchemes[0] != tls.SignatureScheme(0x0403) || hello.SignatureSchemes[3] != tls.SignatureScheme(0x0807) {
		t.Fatalf("signature schemes=%v", hello.SignatureSchemes)
	}
	if len(hello.SupportedPoints) != 3 {
		t.Fatalf("supported points=%v", hello.SupportedPoints)
	}
	if hello.SupportedVersions[0] != tls.VersionTLS13 {
		t.Fatalf("versions=%v", hello.SupportedVersions)
	}
	// distinguishable from Go's default hello
	goDefault := &helloRecorder{}
	goServer, goPool := newRecordingTLSServer(t, goDefault, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer goServer.Close()
	plain := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: goPool}}}
	if r, err := plain.Get(goServer.URL); err == nil {
		r.Body.Close()
	} else {
		t.Fatal(err)
	}
	if len(goDefault.last(t).CipherSuites) == len(hello.CipherSuites) && len(goDefault.last(t).SupportedProtos) == 0 {
		t.Fatal("fingerprinted hello is indistinguishable from Go default")
	}
}

func TestNode24ProfileShapesClientHello(t *testing.T) {
	recorder := &helloRecorder{}
	server, pool := newRecordingTLSServer(t, recorder, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer server.Close()
	response, err := fingerprintedClient(t, pool, "node24").Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	hello := recorder.last(t)
	spec := node24ClientHelloSpec("")
	if len(hello.CipherSuites) != len(spec.CipherSuites) || hello.CipherSuites[0] != 0x1301 || hello.CipherSuites[3] != 0xc02b {
		t.Fatalf("cipher suites=%#x", hello.CipherSuites)
	}
	if len(hello.SupportedProtos) != 1 || hello.SupportedProtos[0] != "http/1.1" {
		t.Fatalf("node24 must advertise http/1.1 ALPN only, got %v", hello.SupportedProtos)
	}
	if len(hello.SupportedCurves) != 3 || len(hello.SignatureSchemes) != 9 || hello.SignatureSchemes[1] != tls.SignatureScheme(0x0804) {
		t.Fatalf("curves=%v sigs=%v", hello.SupportedCurves, hello.SignatureSchemes)
	}
}

func TestUnknownTLSProfileFailsClosedBeforeUpstream(t *testing.T) {
	hits := 0
	server, pool := newRecordingTLSServer(t, &helloRecorder{}, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { hits++ }))
	defer server.Close()
	base := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}
	if _, err := clientForTLSProfile(base, "chrome_999"); !errors.Is(err, ErrUnknownTLSProfile) {
		t.Fatalf("unknown profile error=%v", err)
	}
	if same, err := clientForTLSProfile(base, ""); err != nil || same != base {
		t.Fatalf("empty profile must return the base client unchanged: %v", err)
	}
	if hits != 0 {
		t.Fatal("unknown profile reached upstream")
	}
	for _, name := range []string{"codex_rustls", "node24"} {
		if _, err := lookupTLSProfile(strings.ToUpper(" " + name + " ")); err != nil {
			t.Fatalf("profile lookup should normalize %q: %v", name, err)
		}
	}
}

func TestServerCallUpstreamUsesLeaseTLSFingerprint(t *testing.T) {
	recorder := &helloRecorder{}
	server, pool := newRecordingTLSServer(t, recorder, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"x","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer server.Close()
	gw := NewServer(events.Producer{}, "codex")
	gw.HTTPClient = &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}
	lease := contracts.Lease{AccountID: "a", Provider: "codex", Credential: contracts.Credential{Kind: "static", AccessToken: "k"}, Profile: contracts.UpstreamProfile{BaseURL: server.URL, Protocol: "openai_chat", TLSFingerprint: "codex_rustls"}}
	body, status, usage, networkErr, _, err := gw.callUpstream(context.Background(), lease, ChatCompletionRequest{Model: "m", Messages: []map[string]any{{"role": "user", "content": "hi"}}}, "openai_chat")
	if err != nil || status != http.StatusOK || networkErr || usage == nil || len(body) == 0 {
		t.Fatalf("status=%d networkErr=%t usage=%v err=%v", status, networkErr, usage, err)
	}
	if hello := recorder.last(t); len(hello.CipherSuites) != len(codexRustlsClientHelloSpec("").CipherSuites) || hello.CipherSuites[0] != 0x1302 {
		t.Fatalf("upstream did not see the rustls hello: %#x", hello.CipherSuites)
	}
	// unknown profile on the lease is a pre-flight failure (no bytes sent)
	before := len(recorder.hellos)
	lease.Profile.TLSFingerprint = "unknown_profile"
	_, _, _, networkErr, _, err = gw.callUpstream(context.Background(), lease, ChatCompletionRequest{Model: "m", Messages: []map[string]any{{"role": "user", "content": "hi"}}}, "openai_chat")
	if !errors.Is(err, ErrUnknownTLSProfile) || !networkErr {
		t.Fatalf("unknown lease profile: networkErr=%t err=%v", networkErr, err)
	}
	if len(recorder.hellos) != before {
		t.Fatal("unknown profile still dialed upstream")
	}
}

// TestTLSProfileTunnelsThroughHTTPProxy proves the fingerprinted handshake
// terminates at the origin even when the lease carries a proxy: the proxy
// sees only CONNECT, the origin records the rustls hello.
func TestTLSProfileTunnelsThroughHTTPProxy(t *testing.T) {
	recorder := &helloRecorder{}
	origin, pool := newRecordingTLSServer(t, recorder, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "via-proxy") }))
	defer origin.Close()
	connects := make(chan string, 4)
	proxyListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer proxyListener.Close()
	go func() {
		for {
			conn, err := proxyListener.Accept()
			if err != nil {
				return
			}
			go func(client net.Conn) {
				defer client.Close()
				reader := bufio.NewReader(client)
				request, err := http.ReadRequest(reader)
				if err != nil || request.Method != http.MethodConnect {
					return
				}
				connects <- request.Host
				upstream, err := net.Dial("tcp", request.Host)
				if err != nil {
					_, _ = io.WriteString(client, "HTTP/1.1 502 Bad Gateway\r\n\r\n")
					return
				}
				defer upstream.Close()
				_, _ = io.WriteString(client, "HTTP/1.1 200 Connection established\r\n\r\n")
				done := make(chan struct{}, 2)
				go func() { _, _ = io.Copy(upstream, reader); done <- struct{}{} }()
				go func() { _, _ = io.Copy(client, upstream); done <- struct{}{} }()
				<-done
			}(conn)
		}
	}()
	proxyURL, _ := url.Parse("http://" + proxyListener.Addr().String())
	base := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL), TLSClientConfig: &tls.Config{RootCAs: pool}}, Timeout: 5 * time.Second}
	client, err := clientForTLSProfile(base, "codex_rustls")
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Get(origin.URL)
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if string(payload) != "via-proxy" {
		t.Fatalf("body=%q", payload)
	}
	select {
	case host := <-connects:
		if !strings.HasSuffix(origin.URL, host) {
			t.Fatalf("proxy CONNECT host=%q origin=%q", host, origin.URL)
		}
	case <-time.After(time.Second):
		t.Fatal("request bypassed the proxy")
	}
	if hello := recorder.last(t); hello.CipherSuites[0] != 0x1302 || len(hello.SupportedProtos) != 0 {
		t.Fatalf("origin behind proxy did not see rustls hello: %#x alpn=%v", hello.CipherSuites, hello.SupportedProtos)
	}
}
