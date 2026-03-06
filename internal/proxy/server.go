package proxy

import (
	"encoding/pem"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/elazarl/goproxy"
)

const repoPathPrefix = "/Ashu11-A/Ashu_eggs/main/"

var (
	eggMap = make(map[string]string)
	mapMu  sync.RWMutex
)

// Register associates an IP address with an Egg name for logging purposes.
func Register(ip, eggName string) {
	mapMu.Lock()
	defer mapMu.Unlock()
	eggMap[ip] = eggName
}

type Proxy struct {
	addr       string
	localRoot  string
	requestLog *os.File
	caPath     string
	mu         sync.Mutex
	seq        uint64
	proxy      *goproxy.ProxyHttpServer
}

func New(addr string, localRoot string, logPath string) (*Proxy, error) {
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return nil, err
	}

	caPath := filepath.Join(filepath.Dir(logPath), "proxy-ca.pem")
	if err := exportGoproxyCA(caPath); err != nil {
		f.Close()
		return nil, fmt.Errorf("export proxy CA: %w", err)
	}

	p := &Proxy{
		addr:       addr,
		localRoot:  localRoot,
		requestLog: f,
		caPath:     caPath,
	}

	p.proxy = goproxy.NewProxyHttpServer()
	p.proxy.Verbose = false
	p.proxy.Logger = &proxyLogger{logFile: f, seq: &p.seq, mu: &p.mu}

	// MITM only for raw.githubusercontent.com so we can inspect path and do local redirect
	rawHostRegex := regexp.MustCompile(`(?i)^raw\.githubusercontent\.com:443$`)
	mitmHandler := goproxy.FuncHttpsHandler(func(host string, ctx *goproxy.ProxyCtx) (*goproxy.ConnectAction, string) {
		return goproxy.MitmConnect, host
	})
	p.proxy.OnRequest(goproxy.ReqHostMatches(rawHostRegex)).HandleConnect(mitmHandler)

	// Log all requests and serve local files for raw.githubusercontent.com when available
	p.proxy.OnRequest().DoFunc(p.handleRequest)

	fmt.Printf("Proxy listening on %s, serving local files from %s\n", addr, localRoot)
	fmt.Printf("Proxy CA written to %s (set CURL_CA_BUNDLE for HTTPS MITM)\n", caPath)

	return p, nil
}

// CACertPath returns the path to the proxy CA certificate for container trust.
func (p *Proxy) CACertPath() string {
	return p.caPath
}

func (p *Proxy) logLine(eggTag, message string) {
	n := atomic.AddUint64(&p.seq, 1)
	line := fmt.Sprintf("%d [PROXY] %s%s\n", n, eggTag, message)
	p.mu.Lock()
	p.requestLog.Write([]byte(line))
	p.mu.Unlock()
}

func (p *Proxy) eggTag(r *http.Request) string {
	remoteIP, _, _ := net.SplitHostPort(r.RemoteAddr)
	mapMu.RLock()
	eggName, ok := eggMap[remoteIP]
	mapMu.RUnlock()
	tag := fmt.Sprintf("[IP:%s] ", remoteIP)
	if ok {
		tag += fmt.Sprintf("[%s] ", eggName)
	}
	return tag
}

func (p *Proxy) handleRequest(r *http.Request, ctx *goproxy.ProxyCtx) (*http.Request, *http.Response) {
	eggTag := p.eggTag(r)
	fullURL := r.URL.String()
	if r.Host != "" && !strings.HasPrefix(fullURL, "http") {
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		fullURL = fmt.Sprintf("%s://%s%s", scheme, r.Host, fullURL)
	}
	p.logLine(eggTag, fmt.Sprintf("%s %s", r.Method, fullURL))

	// Check raw.githubusercontent.com for our repo path
	host := r.URL.Host
	if host == "" {
		host = r.Host
	}
	if (strings.HasPrefix(host, "raw.githubusercontent.com") || strings.HasPrefix(host, "raw.githubusercontent")) &&
		strings.HasPrefix(r.URL.Path, repoPathPrefix) {

		relativePath := strings.TrimPrefix(r.URL.Path, repoPathPrefix)
		localPath := filepath.Join(p.localRoot, relativePath)

		if _, err := os.Stat(localPath); err == nil {
			p.logLine(eggTag, fmt.Sprintf("[LOCAL_REDIRECT] URL: %s -> Path: %s", fullURL, relativePath))
			f, err := os.Open(localPath)
			if err != nil {
				p.logLine(eggTag, fmt.Sprintf("[NOT_FOUND_LOCAL] URL: %s (open error: %v)", fullURL, err))
				return r, nil
			}
			info, _ := f.Stat()
			resp := &http.Response{
				StatusCode:    http.StatusOK,
				Proto:         "HTTP/1.1",
				ProtoMajor:    1,
				ProtoMinor:    1,
				Header:        make(http.Header),
				Body:          f,
				ContentLength: info.Size(),
				Request:       r,
			}
			return r, resp
		}
		p.logLine(eggTag, fmt.Sprintf("[NOT_FOUND_LOCAL] URL: %s (Tried local path: %s) - Falling back to upstream", fullURL, localPath))
	}

	return r, nil
}

func (p *Proxy) Start() error {
	return http.ListenAndServe(p.addr, p.proxy)
}

// proxyLogger implements goproxy.Logger to write to our log file.
type proxyLogger struct {
	logFile *os.File
	seq     *uint64
	mu      *sync.Mutex
}

func (l *proxyLogger) Logf(format string, args ...interface{}) {
	// Suppress verbose goproxy logs; our custom logging handles the important parts
}

func (l *proxyLogger) Printf(format string, args ...interface{}) {
	// Suppress verbose goproxy logs
}

// exportGoproxyCA writes the goproxy CA certificate to path for client trust.
func exportGoproxyCA(path string) error {
	cert := goproxy.GoproxyCa
	if len(cert.Certificate) == 0 {
		return fmt.Errorf("goproxy CA has no certificate")
	}
	der := cert.Certificate[0]
	block := &pem.Block{Type: "CERTIFICATE", Bytes: der}
	pemBytes := pem.EncodeToMemory(block)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, pemBytes, 0644)
}
