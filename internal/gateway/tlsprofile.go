package gateway

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	utls "github.com/refraction-networking/utls"
)

// TLS fingerprint registry. UpstreamProfile.TLSFingerprint names a ClientHello
// shape that control promises and gateway must actually produce. An unknown
// name fails closed before any bytes reach the upstream: silently falling back
// to Go's default hello would defeat the whole point of the field.
//
// Profiles are snapshots of an official client's ClientHello (see
// docs/fluxgate-keyhive-overview.md §5). Whether a live upstream still accepts
// a given shape is a [live-gate] question (TODO §C).

var ErrUnknownTLSProfile = errors.New("unknown tls fingerprint profile")

type tlsProfile func(serverName string) utls.ClientHelloSpec

var (
	tlsProfilesMu sync.RWMutex
	tlsProfiles   = map[string]tlsProfile{
		"codex_rustls": codexRustlsClientHelloSpec,
		"node24":       node24ClientHelloSpec,
	}
)

// RegisterTLSProfile adds or replaces a named ClientHello shape.
func RegisterTLSProfile(name string, spec func(serverName string) utls.ClientHelloSpec) {
	tlsProfilesMu.Lock()
	defer tlsProfilesMu.Unlock()
	tlsProfiles[normalizeTLSProfileName(name)] = spec
}

// TLSProfiles lists the registered names.
func TLSProfiles() []string {
	tlsProfilesMu.RLock()
	defer tlsProfilesMu.RUnlock()
	names := make([]string, 0, len(tlsProfiles))
	for name := range tlsProfiles {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func normalizeTLSProfileName(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

func lookupTLSProfile(name string) (tlsProfile, error) {
	tlsProfilesMu.RLock()
	defer tlsProfilesMu.RUnlock()
	spec, ok := tlsProfiles[normalizeTLSProfileName(name)]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownTLSProfile, name)
	}
	return spec, nil
}

// clientForTLSProfile returns a client whose HTTPS connections present the
// named ClientHello. An empty name returns base unchanged (standard crypto/tls).
// When the transport has a proxy, the CONNECT tunnel is established here so
// the fingerprinted handshake still happens end-to-end with the origin;
// http.Transport would otherwise ignore DialTLSContext for proxied HTTPS.
func clientForTLSProfile(base *http.Client, profileName string) (*http.Client, error) {
	if strings.TrimSpace(profileName) == "" {
		return base, nil
	}
	spec, err := lookupTLSProfile(profileName)
	if err != nil {
		return nil, err
	}
	if base == nil {
		base = http.DefaultClient
	}
	var transport *http.Transport
	switch configured := base.Transport.(type) {
	case nil:
		transport = http.DefaultTransport.(*http.Transport).Clone()
	case *http.Transport:
		transport = configured.Clone()
	default:
		return nil, errors.New("tls fingerprint requires an *http.Transport")
	}
	var proxyURL *url.URL
	if transport.Proxy != nil {
		// Resolve the proxy once for the tunnel dialer. All profiles are used
		// against a single upstream host per lease, so per-request proxy
		// selection collapses to this.
		probe, _ := http.NewRequest(http.MethodGet, "https://placeholder.invalid/", nil)
		proxyURL, err = transport.Proxy(probe)
		if err != nil {
			return nil, err
		}
	}
	baseTLS := transport.TLSClientConfig
	dialer := &fingerprintDialer{spec: spec, proxy: proxyURL, baseTLS: baseTLS, plainDialer: transport.DialContext}
	transport.Proxy = nil
	transport.DialTLSContext = dialer.DialTLSContext
	// Registered profiles advertise http/1.1 only; make sure Go's transport
	// does not try to negotiate h2 on top of the custom hello.
	transport.ForceAttemptHTTP2 = false
	transport.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	client := *base
	client.Transport = transport
	return &client, nil
}

type fingerprintDialer struct {
	spec        tlsProfile
	proxy       *url.URL
	baseTLS     *tls.Config
	plainDialer func(ctx context.Context, network, addr string) (net.Conn, error)
}

func (d *fingerprintDialer) dialPlain(ctx context.Context, network, addr string) (net.Conn, error) {
	if d.plainDialer != nil {
		return d.plainDialer(ctx, network, addr)
	}
	var nd net.Dialer
	return nd.DialContext(ctx, network, addr)
}

func (d *fingerprintDialer) DialTLSContext(ctx context.Context, network, addr string) (net.Conn, error) {
	var conn net.Conn
	var err error
	if d.proxy != nil {
		conn, err = d.dialThroughProxy(ctx, network, addr)
	} else {
		conn, err = d.dialPlain(ctx, network, addr)
	}
	if err != nil {
		return nil, err
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
		defer conn.SetDeadline(time.Time{})
	}
	host := tlsServerNameFromAddr(addr)
	config := &utls.Config{ServerName: host, MinVersion: utls.VersionTLS12, MaxVersion: utls.VersionTLS13}
	if d.baseTLS != nil {
		config.RootCAs = d.baseTLS.RootCAs
		config.InsecureSkipVerify = d.baseTLS.InsecureSkipVerify
		if d.baseTLS.ServerName != "" {
			config.ServerName = d.baseTLS.ServerName
		}
	}
	uconn := utls.UClient(conn, config, utls.HelloCustom)
	spec := d.spec(config.ServerName)
	if err := uconn.ApplyPreset(&spec); err != nil {
		_ = conn.Close()
		return nil, err
	}
	if err := uconn.HandshakeContext(ctx); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return uconn, nil
}

// dialThroughProxy opens an HTTP CONNECT tunnel so the fingerprinted
// handshake terminates at the origin, not at the proxy.
func (d *fingerprintDialer) dialThroughProxy(ctx context.Context, network, addr string) (net.Conn, error) {
	proxyAddr := d.proxy.Host
	if _, _, err := net.SplitHostPort(proxyAddr); err != nil {
		if d.proxy.Scheme == "https" {
			proxyAddr = net.JoinHostPort(proxyAddr, "443")
		} else {
			proxyAddr = net.JoinHostPort(proxyAddr, "80")
		}
	}
	conn, err := d.dialPlain(ctx, network, proxyAddr)
	if err != nil {
		return nil, err
	}
	if d.proxy.Scheme == "https" {
		proxyTLS := &tls.Config{ServerName: tlsServerNameFromAddr(proxyAddr)}
		if d.baseTLS != nil {
			proxyTLS.RootCAs = d.baseTLS.RootCAs
			proxyTLS.InsecureSkipVerify = d.baseTLS.InsecureSkipVerify
		}
		tlsConn := tls.Client(conn, proxyTLS)
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			_ = conn.Close()
			return nil, err
		}
		conn = tlsConn
	}
	connect := &http.Request{Method: http.MethodConnect, URL: &url.URL{Opaque: addr}, Host: addr, Header: make(http.Header)}
	if d.proxy.User != nil {
		password, _ := d.proxy.User.Password()
		connect.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(d.proxy.User.Username()+":"+password)))
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	if err := connect.Write(conn); err != nil {
		_ = conn.Close()
		return nil, err
	}
	response, err := http.ReadResponse(bufio.NewReader(conn), connect)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_ = conn.Close()
		return nil, fmt.Errorf("proxy CONNECT %s: %s", addr, response.Status)
	}
	_ = conn.SetDeadline(time.Time{})
	return conn, nil
}

func tlsServerNameFromAddr(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	return strings.Trim(host, "[]")
}

func shouldSendSNI(serverName string) bool {
	return serverName != "" && net.ParseIP(serverName) == nil
}

// codexRustlsClientHelloSpec mirrors the rustls hello sent by the official
// Codex CLI (reference: elucid-relay codex_tls.go, JA3 d39e1be3241d516b1f714bd47c2bc968).
func codexRustlsClientHelloSpec(serverName string) utls.ClientHelloSpec {
	extensions := make([]utls.TLSExtension, 0, 11)
	if shouldSendSNI(serverName) {
		extensions = append(extensions, &utls.SNIExtension{ServerName: serverName})
	}
	extensions = append(extensions,
		&utls.SupportedPointsExtension{SupportedPoints: []uint8{0, 1, 2}},
		&utls.SupportedCurvesExtension{Curves: []utls.CurveID{
			utls.X25519, utls.CurveP256, utls.CurveID(0x001e), utls.CurveP521, utls.CurveP384,
			utls.CurveID(0x0100), utls.CurveID(0x0101), utls.CurveID(0x0102), utls.CurveID(0x0103), utls.CurveID(0x0104),
		}},
		&utls.SessionTicketExtension{},
		&utls.GenericExtension{Id: 22},
		&utls.ExtendedMasterSecretExtension{},
		&utls.SignatureAlgorithmsExtension{SupportedSignatureAlgorithms: []utls.SignatureScheme{
			0x0403, 0x0503, 0x0603, 0x0807, 0x0808, 0x0809, 0x080a, 0x080b, 0x0804, 0x0805, 0x0806,
			0x0401, 0x0501, 0x0601, 0x0303, 0x0301, 0x0302, 0x0402, 0x0502, 0x0602,
		}},
		&utls.SupportedVersionsExtension{Versions: []uint16{utls.VersionTLS13, utls.VersionTLS12}},
		&utls.PSKKeyExchangeModesExtension{Modes: []uint8{utls.PskModeDHE}},
		&utls.KeyShareExtension{KeyShares: []utls.KeyShare{{Group: utls.X25519}}},
		&utls.UtlsPaddingExtension{GetPaddingLen: utls.AlwaysPadToLen(512)},
	)
	return utls.ClientHelloSpec{
		CipherSuites: []uint16{
			0x1302, 0x1303, 0x1301, 0xc02c, 0xc030, 0x009f, 0xcca9, 0xcca8, 0xccaa, 0xc02b, 0xc02f, 0x009e,
			0xc024, 0xc028, 0x006b, 0xc023, 0xc027, 0x0067, 0xc00a, 0xc014, 0x0039, 0xc009, 0xc013, 0x0033,
			0x009d, 0x009c, 0x003d, 0x003c, 0x0035, 0x002f, 0x00ff,
		},
		CompressionMethods: []uint8{0},
		Extensions:         extensions,
		TLSVersMin:         utls.VersionTLS12,
		TLSVersMax:         utls.VersionTLS13,
	}
}

// node24ClientHelloSpec mirrors Node.js 24 (OpenSSL) as used by Claude Code
// (reference: elucid-relay codex_tls.go, JA3 44f88fca027f27bab4bb08d4af15f23e).
func node24ClientHelloSpec(serverName string) utls.ClientHelloSpec {
	extensions := make([]utls.TLSExtension, 0, 12)
	if shouldSendSNI(serverName) {
		extensions = append(extensions, &utls.SNIExtension{ServerName: serverName})
	}
	extensions = append(extensions,
		&utls.ExtendedMasterSecretExtension{},
		&utls.RenegotiationInfoExtension{},
		&utls.SupportedCurvesExtension{Curves: []utls.CurveID{utls.X25519, utls.CurveP256, utls.CurveP384}},
		&utls.SupportedPointsExtension{SupportedPoints: []uint8{0}},
		&utls.SessionTicketExtension{},
		&utls.ALPNExtension{AlpnProtocols: []string{"http/1.1"}},
		&utls.StatusRequestExtension{},
		&utls.SignatureAlgorithmsExtension{SupportedSignatureAlgorithms: []utls.SignatureScheme{
			0x0403, 0x0804, 0x0401, 0x0503, 0x0805, 0x0501, 0x0806, 0x0601, 0x0201,
		}},
		&utls.SCTExtension{},
		&utls.KeyShareExtension{KeyShares: []utls.KeyShare{{Group: utls.X25519}}},
		&utls.PSKKeyExchangeModesExtension{Modes: []uint8{utls.PskModeDHE}},
		&utls.SupportedVersionsExtension{Versions: []uint16{utls.VersionTLS13, utls.VersionTLS12}},
	)
	return utls.ClientHelloSpec{
		CipherSuites: []uint16{
			0x1301, 0x1302, 0x1303, 0xc02b, 0xc02f, 0xc02c, 0xc030, 0xcca9, 0xcca8, 0xc009, 0xc013,
			0xc00a, 0xc014, 0x009c, 0x009d, 0x002f, 0x0035, 0x00ff,
		},
		CompressionMethods: []uint8{0},
		Extensions:         extensions,
		TLSVersMin:         utls.VersionTLS12,
		TLSVersMax:         utls.VersionTLS13,
	}
}
