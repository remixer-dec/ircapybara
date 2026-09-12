package server

import (
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"net"
	"path/filepath"
	"testing"
)

func TestGeneratedCertificateUsesRSAAndLegacyTLS(t *testing.T) {
	dir := t.TempDir()
	certFile := filepath.Join(dir, "server.crt")
	keyFile := filepath.Join(dir, "server.key")

	cfg, err := tlsConfig(certFile, keyFile, "localhost", true)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MinVersion != tls.VersionTLS10 {
		t.Fatalf("MinVersion=%x, want TLS 1.0", cfg.MinVersion)
	}
	if len(cfg.Certificates) != 1 || len(cfg.Certificates[0].Certificate) == 0 {
		t.Fatal("missing generated certificate")
	}
	leaf, err := x509.ParseCertificate(cfg.Certificates[0].Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := leaf.PublicKey.(*rsa.PublicKey); !ok {
		t.Fatalf("generated key type %T, want RSA", leaf.PublicKey)
	}
}

func TestLegacyRSAKeyExchangeSuiteEnabled(t *testing.T) {
	dir := t.TempDir()
	cfg, err := tlsConfig(filepath.Join(dir, "server.crt"), filepath.Join(dir, "server.key"), "localhost", true)
	if err != nil {
		t.Fatal(err)
	}
	want := tls.TLS_RSA_WITH_AES_128_CBC_SHA
	for _, suite := range cfg.CipherSuites {
		if suite == want {
			return
		}
	}
	t.Fatalf("legacy RSA cipher suite 0x%x not enabled", want)
}

func TestTLS10RSAHandshake(t *testing.T) {
	dir := t.TempDir()
	serverCfg, err := tlsConfig(filepath.Join(dir, "server.crt"), filepath.Join(dir, "server.key"), "localhost", true)
	if err != nil {
		t.Fatal(err)
	}

	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()
	defer clientConn.Close()

	serverTLS := tls.Server(serverConn, serverCfg)
	clientTLS := tls.Client(clientConn, &tls.Config{
		InsecureSkipVerify: true, // self-signed test certificate
		MinVersion:         tls.VersionTLS10,
		MaxVersion:         tls.VersionTLS10,
		CipherSuites:       []uint16{tls.TLS_RSA_WITH_AES_128_CBC_SHA},
	})

	serverErr := make(chan error, 1)
	go func() { serverErr <- serverTLS.Handshake() }()
	if err := clientTLS.Handshake(); err != nil {
		t.Fatalf("legacy client handshake failed: %v", err)
	}
	if err := <-serverErr; err != nil {
		t.Fatalf("server handshake failed: %v", err)
	}
	state := clientTLS.ConnectionState()
	if state.Version != tls.VersionTLS10 {
		t.Fatalf("negotiated version 0x%x, want TLS 1.0", state.Version)
	}
	if state.CipherSuite != tls.TLS_RSA_WITH_AES_128_CBC_SHA {
		t.Fatalf("negotiated cipher 0x%x, want RSA AES-128-CBC-SHA", state.CipherSuite)
	}
}
