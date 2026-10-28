package fbtenant

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/quic-go/quic-go"
)

// selfSignedCertPEM issues a throwaway certificate for 127.0.0.1.
func selfSignedCertPEM(t *testing.T) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "doq-test"},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
		DNSNames:     []string{"localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM
}

// TestDoQAgainstLocalServer exercises the DoQ transport against an
// in-process QUIC server speaking RFC 9250 (ALPN "doq", one query per
// length-prefixed stream).
func TestDoQAgainstLocalServer(t *testing.T) {
	certPEM, keyPEM := selfSignedCertPEM(t)
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		NextProtos:   doqALPN,
	}

	udpConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer udpConn.Close()
	addr := udpConn.LocalAddr().(*net.UDPAddr)

	tr := &quic.Transport{Conn: udpConn}
	listener, err := tr.Listen(tlsCfg, &quic.Config{MaxIdleTimeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	answer := new(dns.Msg)
	answer.SetReply(&dns.Msg{Question: []dns.Question{{Name: "doq.test.", Qtype: dns.TypeA}}})
	answer.Answer = append(answer.Answer, &dns.A{
		Hdr: dns.RR_Header{Name: "doq.test.", Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60},
		A:   net.IPv4(203, 0, 113, 7),
	})

	serverErr := make(chan error, 1)
	go func() {
		conn, err := listener.Accept(context.Background())
		if err != nil {
			serverErr <- err
			return
		}
		for {
			stream, err := conn.AcceptStream(context.Background())
			if err != nil {
				serverErr <- err
				return
			}
			go func(stream *quic.Stream) {
				defer stream.Close()
				var hdr [2]byte
				if _, err := io.ReadFull(stream, hdr[:]); err != nil {
					return
				}
				buf := make([]byte, binary.BigEndian.Uint16(hdr[:]))
				if _, err := io.ReadFull(stream, buf); err != nil {
					return
				}
				resp := new(dns.Msg)
				if err := resp.Unpack(buf); err != nil {
					return
				}
				ans := new(dns.Msg)
				ans.SetReply(resp)
				ans.Answer = answer.Answer
				wire, _ := ans.Pack()
				var out [2]byte
				binary.BigEndian.PutUint16(out[:], uint16(len(wire)))
				stream.Write(out[:])
				stream.Write(wire)
			}(stream)
		}
	}()

	spec := &UpstreamSpec{Scheme: SchemeQUIC, Host: "127.0.0.1", Port: addr.Port}
	q := new(dns.Msg)
	q.SetQuestion("doq.test.", dns.TypeA)
	q.RecursionDesired = true

	// trust the self-signed server cert in the client dial
	prevDial := doqDialQUIC
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(certPEM)
	doqDialQUIC = func(ctx context.Context, pc *net.UDPConn, addr net.Addr, tlsCfg *tls.Config, cfg *quic.Config) (*quic.Conn, error) {
		tlsCfg = tlsCfg.Clone()
		tlsCfg.RootCAs = pool
		tlsCfg.InsecureSkipVerify = false
		return (&quic.Transport{Conn: pc}).Dial(ctx, addr, tlsCfg, cfg)
	}
	t.Cleanup(func() { doqDialQUIC = prevDial })

	transport := newDoQTransport()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	resp, err := transport.exchange(ctx, spec, q, marker{})
	if err != nil {
		t.Fatalf("doq exchange: %v (server err: %v)", err, <-serverErr)
	}
	if len(resp.Answer) != 1 {
		t.Fatalf("doq answer: %+v", resp.Answer)
	}
	if a, ok := resp.Answer[0].(*dns.A); !ok || !a.A.Equal(net.IPv4(203, 0, 113, 7)) {
		t.Fatalf("doq wrong record: %+v", resp.Answer[0])
	}
}
