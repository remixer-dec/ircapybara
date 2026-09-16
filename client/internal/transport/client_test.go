package transport

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"
)

func TestReadLineBounded(t *testing.T) {
	in := strings.Repeat("x", MaxLineBytes-2) + "\r\n"
	got, err := readLineBounded(bufio.NewReaderSize(strings.NewReader(in), 1024), MaxLineBytes)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != MaxLineBytes-2 {
		t.Fatalf("unexpected length: %d", len(got))
	}
}

func TestReadLineTooLarge(t *testing.T) {
	in := strings.Repeat("x", MaxLineBytes+1) + "\n"
	_, err := readLineBounded(bufio.NewReaderSize(strings.NewReader(in), 1024), MaxLineBytes)
	if err == nil {
		t.Fatal("expected size error")
	}
}

func TestEmitBackpressurePreservesOrderedEvents(t *testing.T) {
	c := New()
	for i := 0; i < EventBuffer; i++ {
		if !c.emit(Event{Type: EventLine, Line: fmt.Sprintf("line-%d", i)}) {
			t.Fatal("unexpected closed client")
		}
	}
	done := make(chan bool, 1)
	go func() { done <- c.emit(Event{Type: EventLine, Line: "after-buffer"}) }()
	select {
	case <-done:
		t.Fatal("emit should block while the bounded queue is full")
	case <-time.After(25 * time.Millisecond):
	}
	first := <-c.events
	if first.Line != "line-0" {
		t.Fatalf("ordered queue lost its oldest line: %#v", first)
	}
	select {
	case ok := <-done:
		if !ok {
			t.Fatal("emit stopped unexpectedly")
		}
	case <-time.After(time.Second):
		t.Fatal("emit did not resume after queue space became available")
	}
	for i := 1; i < EventBuffer; i++ {
		got := <-c.events
		want := fmt.Sprintf("line-%d", i)
		if got.Line != want {
			t.Fatalf("event order changed: got %q want %q", got.Line, want)
		}
	}
	if got := <-c.events; got.Line != "after-buffer" {
		t.Fatalf("last event changed: %#v", got)
	}
}

func TestSendRejectsOversizedLineBeforeConnectionCheck(t *testing.T) {
	c := New()
	err := c.Send(strings.Repeat("x", MaxLineBytes))
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("expected line-size error, got %v", err)
	}
}

func TestPinnedCertificateAndPublicKey(t *testing.T) {
	certDER, certPEM, pubPEM := makeTestCertificate(t)

	certPins, err := parseTLSPins(certPEM)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyPinnedPeer([][]byte{certDER}, certPins); err != nil {
		t.Fatalf("certificate pin did not match: %v", err)
	}

	keyPins, err := parseTLSPins(pubPEM)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyPinnedPeer([][]byte{certDER}, keyPins); err != nil {
		t.Fatalf("public-key pin did not match: %v", err)
	}
}

func TestPinnedCertificateRejectsDifferentPeer(t *testing.T) {
	_, certPEM, _ := makeTestCertificate(t)
	otherDER, _, _ := makeTestCertificate(t)
	pins, err := parseTLSPins(certPEM)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyPinnedPeer([][]byte{otherDER}, pins); err == nil {
		t.Fatal("expected mismatched peer to be rejected")
	}
}

func TestConnectSelfSignedTrustModes(t *testing.T) {
	t.Run("system verification rejects self-signed", func(t *testing.T) {
		addr, _, stop := startSelfSignedTLSServer(t)
		defer stop()
		c := New()
		err := c.Connect(context.Background(), Config{Address: addr, Timeout: 2 * time.Second})
		if err == nil {
			_ = c.Close()
			t.Fatal("expected system verification to reject self-signed certificate")
		}
	})

	t.Run("explicit self-signed allowance connects", func(t *testing.T) {
		addr, _, stop := startSelfSignedTLSServer(t)
		defer stop()
		c := New()
		if err := c.Connect(context.Background(), Config{Address: addr, Timeout: 2 * time.Second, AllowInsecureTLS: true}); err != nil {
			t.Fatalf("self-signed allowance failed: %v", err)
		}
		_ = c.Close()
	})

	t.Run("certificate pin connects", func(t *testing.T) {
		addr, certPEM, stop := startSelfSignedTLSServer(t)
		defer stop()
		c := New()
		if err := c.Connect(context.Background(), Config{Address: addr, Timeout: 2 * time.Second, PinnedCertificatePEM: certPEM}); err != nil {
			t.Fatalf("certificate pin failed: %v", err)
		}
		_ = c.Close()
	})
}

func startSelfSignedTLSServer(t *testing.T) (string, []byte, func()) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "ircapybara.local"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     []string{"ircapybara.local"},
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tlsConfig := &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: priv}}, MinVersion: tls.VersionTLS12}
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		tlsConn := tls.Server(conn, tlsConfig)
		_ = tlsConn.Handshake()
		_ = tlsConn.Close()
	}()
	stop := func() {
		_ = listener.Close()
		select {
		case <-done:
		case <-time.After(time.Second):
		}
	}
	return listener.Addr().String(), certPEM, stop
}

func makeTestCertificate(t *testing.T) ([]byte, []byte, []byte) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "ircapybara.local"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     []string{"ircapybara.local"},
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	pubDER, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	pubPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER})
	return der, certPEM, pubPEM
}
