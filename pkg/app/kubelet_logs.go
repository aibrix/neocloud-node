package app

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/aibrix/neocloud-node/pkg/core"
	"github.com/aibrix/neocloud-node/pkg/vk"
)

func validateLogOptions(o Options) error {
	if o.LogSnapshotIdle <= 0 || o.LogSnapshotMax <= 0 {
		return errors.New("log snapshot timeouts must be positive")
	}
	if o.KubeletAPIEnabled {
		if o.KubeletAuthMode != "none" {
			return errors.New("kubelet API requires explicit --kubelet-auth-mode=none for isolated development")
		}
		if net.ParseIP(o.PodIP) == nil {
			return errors.New("kubelet API requires a valid --pod-ip or POD_IP")
		}
		if (o.TLSCertFile == "") != (o.TLSKeyFile == "") {
			return errors.New("kubelet API requires both certificate and key when either is supplied")
		}
		_, port, err := net.SplitHostPort(o.ListenAddr)
		if err != nil || port == "0" {
			return errors.New("kubelet API requires a fixed listening port")
		}
	}
	if o.LambdaLogsURL != "" {
		u, err := url.Parse(o.LambdaLogsURL)
		if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.Path != "/neocloud-node/v1/runner/logs" || u.RawQuery != "" || u.Fragment != "" {
			return errors.New("lambda logs URL must be an HTTP(S) runner endpoint ending /neocloud-node/v1/runner/logs")
		}
		if strings.TrimSpace(o.RunnerCallbackSecret) == "" || o.RunnerCallbackListenAddr == "" || o.LambdaCallbackURL == "" {
			return errors.New("Lambda logs require runner callback listener, callback secret and Lambda callback URL")
		}
	}
	return nil
}

func tlsConfig(o Options) (*tls.Config, error) {
	var cert tls.Certificate
	if o.TLSCertFile != "" || o.TLSKeyFile != "" {
		loaded, err := tls.LoadX509KeyPair(o.TLSCertFile, o.TLSKeyFile)
		if err != nil {
			return nil, fmt.Errorf("load kubelet serving certificate: %w", err)
		}
		leaf, err := x509.ParseCertificate(loaded.Certificate[0])
		if err != nil {
			return nil, err
		}
		if err := leaf.VerifyHostname(o.PodIP); err != nil {
			return nil, fmt.Errorf("serving certificate does not cover Pod IP: %w", err)
		}
		if time.Now().Before(leaf.NotBefore) || !time.Now().Before(leaf.NotAfter) {
			return nil, errors.New("kubelet serving certificate is not currently valid")
		}
		cert = loaded
	} else {
		ip := net.ParseIP(o.PodIP)
		if ip == nil {
			return nil, errors.New("invalid kubelet Pod IP")
		}
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return nil, err
		}
		serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
		if err != nil {
			return nil, err
		}
		now := time.Now()
		leaf := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "neocloud-node-kubelet"}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(365 * 24 * time.Hour), IPAddresses: []net.IP{ip}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
		der, err := x509.CreateCertificate(rand.Reader, leaf, leaf, &key.PublicKey, key)
		if err != nil {
			return nil, err
		}
		cert = tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	}
	return &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12, NextProtos: []string{"http/1.1"}}, nil
}

func preserveLogTail(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/containerLogs/") {
			http.NotFound(w, r)
			return
		}
		r = r.WithContext(vk.WithLogTailPresence(r.Context(), r.URL.Query().Has("tailLines")))
		next.ServeHTTP(&logResponseWriter{ResponseWriter: w}, r)
	})
}

// VK's error handler tries to append diagnostics after a failed io.Copy. Once
// output has started, terminate the HTTP stream instead of returning false
// success and mixing controller errors into application output.
type logResponseWriter struct {
	http.ResponseWriter
	committed bool
}

func (w *logResponseWriter) WriteHeader(status int) {
	if w.committed {
		if status >= 400 {
			panic(http.ErrAbortHandler)
		}
		return
	}
	w.committed = true
	w.ResponseWriter.WriteHeader(status)
}
func (w *logResponseWriter) Write(p []byte) (int, error) {
	if !w.committed {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(p)
}
func (w *logResponseWriter) Flush() {
	if !w.committed {
		w.WriteHeader(http.StatusOK)
	}
	_ = http.NewResponseController(w.ResponseWriter).Flush()
}
func (w *logResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func providerLogReaders(primary string, adapter core.ProviderAdapter, others map[string]core.ProviderAdapter) map[string]core.ProviderLogReader {
	out := map[string]core.ProviderLogReader{}
	if reader, ok := adapter.(core.ProviderLogReader); ok {
		out[primary] = reader
	}
	for name, a := range others {
		if reader, ok := a.(core.ProviderLogReader); ok {
			out[name] = reader
		}
	}
	return out
}
