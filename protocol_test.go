// This file was shamefully stolen from github.com/armon/go-proxyproto.
// It has been heavily edited to conform to this lib.
//
// Thanks @armon
package proxyproto

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

func TestPassthrough(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	pl := &Listener{Listener: l}

	cliResult := make(chan error)
	go func() {
		conn, err := net.Dial("tcp", pl.Addr().String())
		if err != nil {
			cliResult <- err
			return
		}
		defer conn.Close()

		if _, err := conn.Write([]byte("ping")); err != nil {
			cliResult <- err
			return
		}
		recv := make([]byte, 4)
		if _, err = conn.Read(recv); err != nil {
			cliResult <- err
			return
		}
		if !bytes.Equal(recv, []byte("pong")) {
			cliResult <- fmt.Errorf("bad: %v", recv)
			return
		}
		close(cliResult)
	}()

	conn, err := pl.Accept()
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	defer conn.Close()

	recv := make([]byte, 4)
	_, err = conn.Read(recv)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !bytes.Equal(recv, []byte("ping")) {
		t.Fatalf("bad: %v", recv)
	}

	if _, err := conn.Write([]byte("pong")); err != nil {
		t.Fatalf("err: %v", err)
	}
	err = <-cliResult
	if err != nil {
		t.Fatalf("client error: %v", err)
	}
}

// TestRequiredWithReadHeaderTimeout will iterate through 3 different timeouts to see
// whether using a REQUIRE policy for a listener would cause an error if the timeout
// is triggerred without a proxy protocol header being defined.
func TestRequiredWithReadHeaderTimeout(t *testing.T) {
	for _, duration := range []int{100, 200, 400} {
		t.Run(fmt.Sprint(duration), func(t *testing.T) {
			start := time.Now()

			l, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatalf("err: %v", err)
			}

			pl := &Listener{
				Listener:          l,
				ReadHeaderTimeout: time.Millisecond * time.Duration(duration),
				Policy: func(upstream net.Addr) (Policy, error) {
					return REQUIRE, nil
				},
			}

			cliResult := make(chan error)
			go func() {
				conn, err := net.Dial("tcp", pl.Addr().String())
				if err != nil {
					cliResult <- err
					return
				}
				defer conn.Close()

				close(cliResult)
			}()

			conn, err := pl.Accept()
			if err != nil {
				t.Fatalf("err: %v", err)
			}
			defer conn.Close()

			// Read blocks forever if there is no ReadHeaderTimeout and the policy is not REQUIRE
			recv := make([]byte, 4)
			_, err = conn.Read(recv)

			if err != nil && !errors.Is(err, ErrNoProxyProtocol) && time.Since(start)-pl.ReadHeaderTimeout > 10*time.Millisecond {
				t.Fatal("proxy proto should not be found and time should be close to read timeout")
			}
			err = <-cliResult
			if err != nil {
				t.Fatalf("client error: %v", err)
			}
		})
	}
}

// TestUseWithReadHeaderTimeout will iterate through 3 different timeouts to see
// whether using a USE policy for a listener would not cause an error if the timeout
// is triggerred without a proxy protocol header being defined.
func TestUseWithReadHeaderTimeout(t *testing.T) {
	for _, duration := range []int{100, 200, 400} {
		t.Run(fmt.Sprint(duration), func(t *testing.T) {
			start := time.Now()

			l, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatalf("err: %v", err)
			}

			pl := &Listener{
				Listener:          l,
				ReadHeaderTimeout: time.Millisecond * time.Duration(duration),
				Policy: func(upstream net.Addr) (Policy, error) {
					return USE, nil
				},
			}

			cliResult := make(chan error)
			go func() {
				conn, err := net.Dial("tcp", pl.Addr().String())
				if err != nil {
					cliResult <- err
					return
				}
				defer conn.Close()

				close(cliResult)
			}()

			conn, err := pl.Accept()
			if err != nil {
				t.Fatalf("err: %v", err)
			}
			defer conn.Close()

			// 2 times the ReadHeaderTimeout because the first timeout
			// should occur (the one set on the listener) and allow for the second to follow up
			if err := conn.SetDeadline(time.Now().Add(pl.ReadHeaderTimeout * 2)); err != nil {
				t.Fatalf("err: %v", err)
			}

			// Read blocks forever if there is no ReadHeaderTimeout
			recv := make([]byte, 4)
			_, err = conn.Read(recv)

			if err != nil && !errors.Is(err, ErrNoProxyProtocol) && (time.Since(start)-(pl.ReadHeaderTimeout*2)) > 10*time.Millisecond {
				t.Fatal("proxy proto should not be found and time should be close to read timeout")
			}
			err = <-cliResult
			if err != nil {
				t.Fatalf("client error: %v", err)
			}
		})
	}
}

func TestReadHeaderTimeoutIsReset(t *testing.T) {
	const timeout = time.Millisecond * 250

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	pl := &Listener{
		Listener:          l,
		ReadHeaderTimeout: timeout,
	}

	header := &Header{
		Version:           2,
		Command:           PROXY,
		TransportProtocol: TCPv4,
		SourceAddr: &net.TCPAddr{
			IP:   net.ParseIP("10.1.1.1"),
			Port: 1000,
		},
		DestinationAddr: &net.TCPAddr{
			IP:   net.ParseIP("20.2.2.2"),
			Port: 2000,
		},
	}

	cliResult := make(chan error)
	go func() {
		conn, err := net.Dial("tcp", pl.Addr().String())
		if err != nil {
			cliResult <- err
			return
		}
		defer conn.Close()

		// Write out the header!
		if _, err := header.WriteTo(conn); err != nil {
			cliResult <- err
			return
		}

		// Sleep here longer than the configured timeout.
		time.Sleep(timeout * 2)

		if _, err := conn.Write([]byte("ping")); err != nil {
			cliResult <- err
			return
		}
		recv := make([]byte, 4)
		if _, err := conn.Read(recv); err != nil {
			cliResult <- err
			return
		}
		if !bytes.Equal(recv, []byte("pong")) {
			cliResult <- fmt.Errorf("bad: %v", recv)
			return
		}
		close(cliResult)
	}()

	conn, err := pl.Accept()
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	defer conn.Close()

	// Set our deadlines higher than our ReadHeaderTimeout
	if err := conn.SetReadDeadline(time.Now().Add(timeout * 3)); err != nil {
		t.Fatalf("err: %v", err)
	}
	if err := conn.SetWriteDeadline(time.Now().Add(timeout * 3)); err != nil {
		t.Fatalf("err: %v", err)
	}

	recv := make([]byte, 4)
	if _, err = conn.Read(recv); err != nil {
		t.Fatalf("err: %v", err)
	}
	if !bytes.Equal(recv, []byte("ping")) {
		t.Fatalf("bad: %v", recv)
	}

	if _, err := conn.Write([]byte("pong")); err != nil {
		t.Fatalf("err: %v", err)
	}

	// Check the remote addr
	addr := conn.RemoteAddr().(*net.TCPAddr)
	if addr.IP.String() != "10.1.1.1" {
		t.Fatalf("bad: %v", addr)
	}
	if addr.Port != 1000 {
		t.Fatalf("bad: %v", addr)
	}

	h := conn.(*Conn).ProxyHeader()
	if !h.EqualsTo(header) {
		t.Errorf("bad: %v", h)
	}
	err = <-cliResult
	if err != nil {
		t.Fatalf("client error: %v", err)
	}
}

// TestReadHeaderTimeoutIsEmpty ensures the default is set if it is empty.
// The default is 10s, but we delay sending a message, so use 200ms in this test.
// We expect the actual address and port to be returned,
// rather than the ProxyHeader we defined.
func TestReadHeaderTimeoutIsEmpty(t *testing.T) {
	DefaultReadHeaderTimeout = 200 * time.Millisecond

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	pl := &Listener{
		Listener: l,
	}

	header := &Header{
		Version:           2,
		Command:           PROXY,
		TransportProtocol: TCPv4,
		SourceAddr: &net.TCPAddr{
			IP:   net.ParseIP("10.1.1.1"),
			Port: 1000,
		},
		DestinationAddr: &net.TCPAddr{
			IP:   net.ParseIP("20.2.2.2"),
			Port: 2000,
		},
	}

	cliResult := make(chan error)
	go func() {
		conn, err := net.Dial("tcp", pl.Addr().String())
		if err != nil {
			cliResult <- err
			return
		}
		defer conn.Close()

		// Sleep here longer than the configured timeout.
		time.Sleep(250 * time.Millisecond)

		// Write out the header!
		if _, err := header.WriteTo(conn); err != nil {
			cliResult <- err
			return
		}

		if _, err := conn.Write([]byte("ping")); err != nil {
			cliResult <- err
			return
		}

		close(cliResult)
	}()

	conn, err := pl.Accept()
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	defer conn.Close()

	recv := make([]byte, 4)
	if _, err = conn.Read(recv); err != nil {
		t.Fatalf("err: %v", err)
	}

	// Check the remote addr
	addr := conn.RemoteAddr().(*net.TCPAddr)
	if addr.IP.String() == "10.1.1.1" {
		t.Fatalf("bad: %v", addr)
	}
	if addr.Port == 1000 {
		t.Fatalf("bad: %v", addr)
	}
	err = <-cliResult
	if err != nil {
		t.Fatalf("client error: %v", err)
	}
}

// TestReadHeaderTimeoutIsNegative does the same as above except
// with a negative timeout. Therefore, we expect the right ProxyHeader
// to be returned.
func TestReadHeaderTimeoutIsNegative(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	pl := &Listener{
		Listener:          l,
		ReadHeaderTimeout: -1,
	}

	header := &Header{
		Version:           2,
		Command:           PROXY,
		TransportProtocol: TCPv4,
		SourceAddr: &net.TCPAddr{
			IP:   net.ParseIP("10.1.1.1"),
			Port: 1000,
		},
		DestinationAddr: &net.TCPAddr{
			IP:   net.ParseIP("20.2.2.2"),
			Port: 2000,
		},
	}

	cliResult := make(chan error)
	go func() {
		conn, err := net.Dial("tcp", pl.Addr().String())
		if err != nil {
			cliResult <- err
			return
		}
		defer conn.Close()

		// Sleep here longer than the configured timeout.
		time.Sleep(250 * time.Millisecond)

		// Write out the header!
		if _, err := header.WriteTo(conn); err != nil {
			cliResult <- err
			return
		}

		if _, err := conn.Write([]byte("ping")); err != nil {
			cliResult <- err
			return
		}

		close(cliResult)
	}()

	conn, err := pl.Accept()
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	defer conn.Close()

	recv := make([]byte, 4)
	if _, err = conn.Read(recv); err != nil {
		t.Fatalf("err: %v", err)
	}

	// Check the remote addr
	addr := conn.RemoteAddr().(*net.TCPAddr)
	if addr.IP.String() != "10.1.1.1" {
		t.Fatalf("bad: %v", addr)
	}
	if addr.Port != 1000 {
		t.Fatalf("bad: %v", addr)
	}
	err = <-cliResult
	if err != nil {
		t.Fatalf("client error: %v", err)
	}
}

func TestParse_ipv4(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	pl := &Listener{Listener: l}

	header := &Header{
		Version:           2,
		Command:           PROXY,
		TransportProtocol: TCPv4,
		SourceAddr: &net.TCPAddr{
			IP:   net.ParseIP("10.1.1.1"),
			Port: 1000,
		},
		DestinationAddr: &net.TCPAddr{
			IP:   net.ParseIP("20.2.2.2"),
			Port: 2000,
		},
	}

	cliResult := make(chan error)
	go func() {
		conn, err := net.Dial("tcp", pl.Addr().String())
		if err != nil {
			cliResult <- err
			return
		}
		defer conn.Close()

		// Write out the header!
		if _, err := header.WriteTo(conn); err != nil {
			cliResult <- err
			return
		}

		if _, err := conn.Write([]byte("ping")); err != nil {
			cliResult <- err
			return
		}

		recv := make([]byte, 4)
		if _, err = conn.Read(recv); err != nil {
			cliResult <- err
			return
		}
		if !bytes.Equal(recv, []byte("pong")) {
			cliResult <- fmt.Errorf("bad: %v", recv)
			return
		}
		close(cliResult)
	}()

	conn, err := pl.Accept()
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	defer conn.Close()

	recv := make([]byte, 4)
	if _, err = conn.Read(recv); err != nil {
		t.Fatalf("err: %v", err)
	}
	if !bytes.Equal(recv, []byte("ping")) {
		t.Fatalf("bad: %v", recv)
	}

	if _, err := conn.Write([]byte("pong")); err != nil {
		t.Fatalf("err: %v", err)
	}

	// Check the remote addr
	addr := conn.RemoteAddr().(*net.TCPAddr)
	if addr.IP.String() != "10.1.1.1" {
		t.Fatalf("bad: %v", addr)
	}
	if addr.Port != 1000 {
		t.Fatalf("bad: %v", addr)
	}

	h := conn.(*Conn).ProxyHeader()
	if !h.EqualsTo(header) {
		t.Errorf("bad: %v", h)
	}
	err = <-cliResult
	if err != nil {
		t.Fatalf("client error: %v", err)
	}
}

func TestParse_ipv6(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	pl := &Listener{Listener: l}

	header := &Header{
		Version:           2,
		Command:           PROXY,
		TransportProtocol: TCPv6,
		SourceAddr: &net.TCPAddr{
			IP:   net.ParseIP("ffff::ffff"),
			Port: 1000,
		},
		DestinationAddr: &net.TCPAddr{
			IP:   net.ParseIP("ffff::ffff"),
			Port: 2000,
		},
	}

	cliResult := make(chan error)
	go func() {
		conn, err := net.Dial("tcp", pl.Addr().String())
		if err != nil {
			cliResult <- err
			return
		}
		defer conn.Close()

		// Write out the header!
		if _, err := header.WriteTo(conn); err != nil {
			cliResult <- err
			return
		}

		if _, err := conn.Write([]byte("ping")); err != nil {
			cliResult <- err
			return
		}

		recv := make([]byte, 4)
		if _, err = conn.Read(recv); err != nil {
			cliResult <- err
			return
		}
		if !bytes.Equal(recv, []byte("pong")) {
			cliResult <- fmt.Errorf("bad: %v", recv)
			return
		}
		close(cliResult)
	}()

	conn, err := pl.Accept()
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	defer conn.Close()

	recv := make([]byte, 4)
	if _, err = conn.Read(recv); err != nil {
		t.Fatalf("err: %v", err)
	}
	if !bytes.Equal(recv, []byte("ping")) {
		t.Fatalf("bad: %v", recv)
	}

	if _, err := conn.Write([]byte("pong")); err != nil {
		t.Fatalf("err: %v", err)
	}

	// Check the remote addr
	addr := conn.RemoteAddr().(*net.TCPAddr)
	if addr.IP.String() != "ffff::ffff" {
		t.Fatalf("bad: %v", addr)
	}
	if addr.Port != 1000 {
		t.Fatalf("bad: %v", addr)
	}

	h := conn.(*Conn).ProxyHeader()
	if !h.EqualsTo(header) {
		t.Errorf("bad: %v", h)
	}
	err = <-cliResult
	if err != nil {
		t.Fatalf("client error: %v", err)
	}
}

func TestAcceptReturnsErrorWhenPolicyFuncErrors(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	expectedErr := fmt.Errorf("failure")
	policyFunc := func(upstream net.Addr) (Policy, error) { return USE, expectedErr }

	pl := &Listener{Listener: l, Policy: policyFunc}

	cliResult := make(chan error)
	go func() {
		conn, err := net.Dial("tcp", pl.Addr().String())
		if err != nil {
			cliResult <- err
			return
		}
		defer conn.Close()

		close(cliResult)
	}()

	conn, err := pl.Accept()
	if err != expectedErr {
		t.Fatalf("Expected error %v, got %v", expectedErr, err)
	}

	if conn != nil {
		t.Fatalf("Expected no connection, got %v", conn)
	}
	err = <-cliResult
	if err != nil {
		t.Fatalf("client error: %v", err)
	}
}

func TestPanicIfPolicyAndConnPolicySet(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	connPolicyFunc := func(connopts ConnPolicyOptions) (Policy, error) { return USE, nil }
	policyFunc := func(upstream net.Addr) (Policy, error) { return USE, nil }

	pl := &Listener{Listener: l, ConnPolicy: connPolicyFunc, Policy: policyFunc}

	cliResult := make(chan error)
	go func() {
		conn, err := net.Dial("tcp", pl.Addr().String())
		if err != nil {
			cliResult <- err
			return
		}
		defer conn.Close()

		close(cliResult)
	}()

	defer func() {
		if r := recover(); r != nil {
			fmt.Printf("accept did panic as expected with error, %v", r)
		}
	}()
	conn, err := pl.Accept()
	if err != nil {
		t.Fatalf("Expected the accept to panic but did not and error is returned, got %v", err)
	}

	if conn != nil {
		t.Fatalf("xpected the accept to panic but did not, got %v", conn)
	}
	t.Fatalf("expected the accept to panic but did not")
}

func TestAcceptReturnsErrorWhenConnPolicyFuncErrors(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	expectedErr := fmt.Errorf("failure")
	connPolicyFunc := func(connopts ConnPolicyOptions) (Policy, error) { return USE, expectedErr }

	pl := &Listener{Listener: l, ConnPolicy: connPolicyFunc}

	cliResult := make(chan error)
	go func() {
		conn, err := net.Dial("tcp", pl.Addr().String())
		if err != nil {
			cliResult <- err
			return
		}
		defer conn.Close()

		close(cliResult)
	}()

	conn, err := pl.Accept()
	if err != expectedErr {
		t.Fatalf("Expected error %v, got %v", expectedErr, err)
	}

	if conn != nil {
		t.Fatalf("Expected no connection, got %v", conn)
	}
	err = <-cliResult
	if err != nil {
		t.Fatalf("client error: %v", err)
	}
}

func TestReadingIsRefusedWhenProxyHeaderRequiredButMissing(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	policyFunc := func(upstream net.Addr) (Policy, error) { return REQUIRE, nil }

	pl := &Listener{Listener: l, Policy: policyFunc}

	cliResult := make(chan error)
	go func() {
		conn, err := net.Dial("tcp", pl.Addr().String())
		if err != nil {
			cliResult <- err
			return
		}
		defer conn.Close()

		if _, err := conn.Write([]byte("ping")); err != nil {
			cliResult <- err
			return
		}

		close(cliResult)
	}()

	conn, err := pl.Accept()
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	defer conn.Close()

	recv := make([]byte, 4)
	if _, err = conn.Read(recv); err != ErrNoProxyProtocol {
		t.Fatalf("Expected error %v, received %v", ErrNoProxyProtocol, err)
	}
	err = <-cliResult
	if err != nil {
		t.Fatalf("client error: %v", err)
	}
}

func TestReadingIsRefusedWhenProxyHeaderPresentButNotAllowed(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	policyFunc := func(upstream net.Addr) (Policy, error) { return REJECT, nil }

	pl := &Listener{Listener: l, Policy: policyFunc}

	cliResult := make(chan error)
	go func() {
		conn, err := net.Dial("tcp", pl.Addr().String())
		if err != nil {
			cliResult <- err
			return
		}
		defer conn.Close()
		header := &Header{
			Version:           2,
			Command:           PROXY,
			TransportProtocol: TCPv4,
			SourceAddr: &net.TCPAddr{
				IP:   net.ParseIP("10.1.1.1"),
				Port: 1000,
			},
			DestinationAddr: &net.TCPAddr{
				IP:   net.ParseIP("20.2.2.2"),
				Port: 2000,
			},
		}
		if _, err := header.WriteTo(conn); err != nil {
			cliResult <- err
			return
		}

		close(cliResult)
	}()

	conn, err := pl.Accept()
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	defer conn.Close()

	recv := make([]byte, 4)
	if _, err = conn.Read(recv); err != ErrSuperfluousProxyHeader {
		t.Fatalf("Expected error %v, received %v", ErrSuperfluousProxyHeader, err)
	}
	err = <-cliResult
	if err != nil {
		t.Fatalf("client error: %v", err)
	}
}

func TestIgnorePolicyIgnoresIpFromProxyHeader(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	policyFunc := func(upstream net.Addr) (Policy, error) { return IGNORE, nil }

	pl := &Listener{Listener: l, Policy: policyFunc}

	cliResult := make(chan error)
	go func() {
		conn, err := net.Dial("tcp", pl.Addr().String())
		if err != nil {
			cliResult <- err
			return
		}
		defer conn.Close()

		// Write out the header!
		header := &Header{
			Version:           2,
			Command:           PROXY,
			TransportProtocol: TCPv4,
			SourceAddr: &net.TCPAddr{
				IP:   net.ParseIP("10.1.1.1"),
				Port: 1000,
			},
			DestinationAddr: &net.TCPAddr{
				IP:   net.ParseIP("20.2.2.2"),
				Port: 2000,
			},
		}
		if _, err := header.WriteTo(conn); err != nil {
			cliResult <- err
			return
		}

		if _, err := conn.Write([]byte("ping")); err != nil {
			cliResult <- err
			return
		}

		recv := make([]byte, 4)
		if _, err = conn.Read(recv); err != nil {
			cliResult <- err
			return
		}
		if !bytes.Equal(recv, []byte("pong")) {
			cliResult <- fmt.Errorf("bad: %v", recv)
			return
		}

		close(cliResult)
	}()

	conn, err := pl.Accept()
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	defer conn.Close()

	recv := make([]byte, 4)
	if _, err = conn.Read(recv); err != nil {
		t.Fatalf("err: %v", err)
	}
	if !bytes.Equal(recv, []byte("ping")) {
		t.Fatalf("bad: %v", recv)
	}

	if _, err := conn.Write([]byte("pong")); err != nil {
		t.Fatalf("err: %v", err)
	}

	// Check the remote addr
	addr := conn.RemoteAddr().(*net.TCPAddr)
	if addr.IP.String() != "127.0.0.1" {
		t.Fatalf("bad: %v", addr)
	}
	err = <-cliResult
	if err != nil {
		t.Fatalf("client error: %v", err)
	}
}

func Test_AllOptionsAreRecognized(t *testing.T) {
	recognizedOpt1 := false
	opt1 := func(c *Conn) {
		recognizedOpt1 = true
	}

	recognizedOpt2 := false
	opt2 := func(c *Conn) {
		recognizedOpt2 = true
	}

	server, client := net.Pipe()
	defer func() {
		client.Close()
	}()

	c := NewConn(server, opt1, opt2)
	if !recognizedOpt1 {
		t.Error("Expected option 1 recognized")
	}

	if !recognizedOpt2 {
		t.Error("Expected option 2 recognized")
	}

	c.Close()
}

func TestReadingIsRefusedOnErrorWhenRemoteAddrRequestedFirst(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	policyFunc := func(upstream net.Addr) (Policy, error) { return REQUIRE, nil }

	pl := &Listener{Listener: l, Policy: policyFunc}

	cliResult := make(chan error)
	go func() {
		conn, err := net.Dial("tcp", pl.Addr().String())
		if err != nil {
			cliResult <- err
			return
		}
		defer conn.Close()

		if _, err := conn.Write([]byte("ping")); err != nil {
			cliResult <- err
			return
		}

		close(cliResult)
	}()

	conn, err := pl.Accept()
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	defer conn.Close()

	_ = conn.RemoteAddr()
	recv := make([]byte, 4)
	if _, err = conn.Read(recv); err != ErrNoProxyProtocol {
		t.Fatalf("Expected error %v, received %v", ErrNoProxyProtocol, err)
	}
	err = <-cliResult
	if err != nil {
		t.Fatalf("client error: %v", err)
	}
}

func TestReadingIsRefusedOnErrorWhenLocalAddrRequestedFirst(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	policyFunc := func(upstream net.Addr) (Policy, error) { return REQUIRE, nil }

	pl := &Listener{Listener: l, Policy: policyFunc}

	cliResult := make(chan error)
	go func() {
		conn, err := net.Dial("tcp", pl.Addr().String())
		if err != nil {
			cliResult <- err
			return
		}
		defer conn.Close()

		if _, err := conn.Write([]byte("ping")); err != nil {
			cliResult <- err
			return
		}

		close(cliResult)
	}()

	conn, err := pl.Accept()
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	defer conn.Close()

	_ = conn.LocalAddr()
	recv := make([]byte, 4)
	if _, err = conn.Read(recv); err != ErrNoProxyProtocol {
		t.Fatalf("Expected error %v, received %v", ErrNoProxyProtocol, err)
	}
	err = <-cliResult
	if err != nil {
		t.Fatalf("client error: %v", err)
	}
}

func TestSkipProxyProtocolPolicy(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	connPolicyFunc := func(connopts ConnPolicyOptions) (Policy, error) { return SKIP, nil }

	pl := &Listener{
		Listener:   l,
		ConnPolicy: connPolicyFunc,
	}

	cliResult := make(chan error)
	ping := []byte("ping")
	go func() {
		conn, err := net.Dial("tcp", pl.Addr().String())
		if err != nil {
			cliResult <- err
			return
		}
		defer conn.Close()

		if _, err := conn.Write(ping); err != nil {
			cliResult <- err
			return
		}

		close(cliResult)
	}()

	conn, err := pl.Accept()
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	defer conn.Close()

	_, ok := conn.(*net.TCPConn)
	if !ok {
		t.Fatal("err: should be a tcp connection")
	}
	_ = conn.LocalAddr()
	recv := make([]byte, 4)
	if _, err = conn.Read(recv); err != nil {
		t.Fatalf("Unexpected read error: %v", err)
	}

	if !bytes.Equal(ping, recv) {
		t.Fatalf("Unexpected %s data while expected %s", recv, ping)
	}

	err = <-cliResult
	if err != nil {
		t.Fatalf("client error: %v", err)
	}
}

func TestSkipProxyProtocolConnPolicy(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	policyFunc := func(upstream net.Addr) (Policy, error) { return SKIP, nil }

	pl := &Listener{
		Listener: l,
		Policy:   policyFunc,
	}

	cliResult := make(chan error)
	ping := []byte("ping")
	go func() {
		conn, err := net.Dial("tcp", pl.Addr().String())
		if err != nil {
			cliResult <- err
			return
		}
		defer conn.Close()

		if _, err := conn.Write(ping); err != nil {
			cliResult <- err
			return
		}

		close(cliResult)
	}()

	conn, err := pl.Accept()
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	defer conn.Close()

	_, ok := conn.(*net.TCPConn)
	if !ok {
		t.Fatal("err: should be a tcp connection")
	}
	_ = conn.LocalAddr()
	recv := make([]byte, 4)
	if _, err = conn.Read(recv); err != nil {
		t.Fatalf("Unexpected read error: %v", err)
	}

	if !bytes.Equal(ping, recv) {
		t.Fatalf("Unexpected %s data while expected %s", recv, ping)
	}

	err = <-cliResult
	if err != nil {
		t.Fatalf("client error: %v", err)
	}
}

func Test_ConnectionCasts(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	policyFunc := func(upstream net.Addr) (Policy, error) { return REQUIRE, nil }

	pl := &Listener{Listener: l, Policy: policyFunc}

	cliResult := make(chan error)
	go func() {
		conn, err := net.Dial("tcp", pl.Addr().String())
		if err != nil {
			cliResult <- err
			return
		}
		defer conn.Close()

		if _, err := conn.Write([]byte("ping")); err != nil {
			cliResult <- err
			return
		}

		close(cliResult)
	}()

	conn, err := pl.Accept()
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	defer conn.Close()

	proxyprotoConn := conn.(*Conn)
	_, ok := proxyprotoConn.TCPConn()
	if !ok {
		t.Fatal("err: should be a tcp connection")
	}
	_, ok = proxyprotoConn.UDPConn()
	if ok {
		t.Fatal("err: should be a tcp connection not udp")
	}
	_, ok = proxyprotoConn.UnixConn()
	if ok {
		t.Fatal("err: should be a tcp connection not unix")
	}
	_, ok = proxyprotoConn.Raw().(*net.TCPConn)
	if !ok {
		t.Fatal("err: should be a tcp connection")
	}
	err = <-cliResult
	if err != nil {
		t.Fatalf("client error: %v", err)
	}
}

func Test_ConnectionErrorsWhenHeaderValidationFails(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	validationError := fmt.Errorf("failed to validate")
	pl := &Listener{Listener: l, ValidateHeader: func(*Header) error { return validationError }}

	cliResult := make(chan error)
	go func() {
		conn, err := net.Dial("tcp", pl.Addr().String())
		if err != nil {
			cliResult <- err
			return
		}
		defer conn.Close()

		// Write out the header!
		header := &Header{
			Version:           2,
			Command:           PROXY,
			TransportProtocol: TCPv4,
			SourceAddr: &net.TCPAddr{
				IP:   net.ParseIP("10.1.1.1"),
				Port: 1000,
			},
			DestinationAddr: &net.TCPAddr{
				IP:   net.ParseIP("20.2.2.2"),
				Port: 2000,
			},
		}
		if _, err := header.WriteTo(conn); err != nil {
			cliResult <- err
			return
		}

		close(cliResult)
	}()

	conn, err := pl.Accept()
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	defer conn.Close()

	recv := make([]byte, 4)
	if _, err = conn.Read(recv); err != validationError {
		t.Fatalf("expected validation error, got %v", err)
	}
	err = <-cliResult
	if err != nil {
		t.Fatalf("client error: %v", err)
	}
}

func Test_ConnectionHandlesInvalidUpstreamError(t *testing.T) {
	l, err := net.Listen("tcp", "localhost:8080")
	if err != nil {
		t.Fatalf("error creating listener: %v", err)
	}

	var connectionCounter atomic.Int32

	newLn := &Listener{
		Listener: l,
		ConnPolicy: func(_ ConnPolicyOptions) (Policy, error) {
			// Return the invalid upstream error on the first call, the listener
			// should remain open and accepting.
			times := connectionCounter.Load()
			if times == 0 {
				connectionCounter.Store(times + 1)
				return REJECT, ErrInvalidUpstream
			}

			return REJECT, ErrNoProxyProtocol
		},
	}

	// Kick off the listener and return any error via the chanel.
	errCh := make(chan error)
	defer close(errCh)
	go func(t *testing.T) {
		_, err := newLn.Accept()
		errCh <- err
	}(t)

	// Make two calls to trigger the listener's accept, the first should experience
	// the ErrInvalidUpstream and keep the listener open, the second should experience
	// a different error which will cause the listener to close.
	_, _ = http.Get("http://localhost:8080")
	// Wait a few seconds to ensure we didn't get anything back on our channel.
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("invalid upstream shouldn't return an error: %v", err)
		}
	case <-time.After(2 * time.Second):
		// No error returned (as expected, we're still listening though)
	}

	_, _ = http.Get("http://localhost:8080")
	// Wait a few seconds before we fail the test as we should have received an
	// error that was not invalid upstream.
	select {
	case err := <-errCh:
		if err == nil {
			t.Fatalf("errors other than invalid upstream should error")
		}
		if !errors.Is(ErrNoProxyProtocol, err) {
			t.Fatalf("unexpected error type: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for listener")
	}
}

type TestTLSServer struct {
	Listener net.Listener

	// TLS is the optional TLS configuration, populated with a new config
	// after TLS is started. If set on an unstarted server before StartTLS
	// is called, existing fields are copied into the new config.
	TLS             *tls.Config
	TLSClientConfig *tls.Config

	// certificate is a parsed version of the TLS config certificate, if present.
	certificate *x509.Certificate
}

func (s *TestTLSServer) Addr() string {
	return s.Listener.Addr().String()
}

func (s *TestTLSServer) Close() {
	s.Listener.Close()
}

// based on net/http/httptest/Server.StartTLS
func NewTestTLSServer(l net.Listener) *TestTLSServer {
	s := &TestTLSServer{}

	cert, err := tls.X509KeyPair(LocalhostCert, LocalhostKey)
	if err != nil {
		panic(fmt.Sprintf("httptest: NewTLSServer: %v", err))
	}
	s.TLS = new(tls.Config)
	if len(s.TLS.Certificates) == 0 {
		s.TLS.Certificates = []tls.Certificate{cert}
	}
	s.certificate, err = x509.ParseCertificate(s.TLS.Certificates[0].Certificate[0])
	if err != nil {
		panic(fmt.Sprintf("NewTestTLSServer: %v", err))
	}
	certpool := x509.NewCertPool()
	certpool.AddCert(s.certificate)
	s.TLSClientConfig = &tls.Config{
		RootCAs: certpool,
	}
	s.Listener = tls.NewListener(l, s.TLS)

	return s
}

func Test_TLSServer(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	s := NewTestTLSServer(l)
	s.Listener = &Listener{
		Listener: s.Listener,
		Policy: func(upstream net.Addr) (Policy, error) {
			return REQUIRE, nil
		},
	}
	defer s.Close()

	cliResult := make(chan error)
	go func() {
		conn, err := tls.Dial("tcp", s.Addr(), s.TLSClientConfig)
		if err != nil {
			cliResult <- err
			return
		}
		defer conn.Close()

		// Write out the header!
		header := &Header{
			Version:           2,
			Command:           PROXY,
			TransportProtocol: TCPv4,
			SourceAddr: &net.TCPAddr{
				IP:   net.ParseIP("10.1.1.1"),
				Port: 1000,
			},
			DestinationAddr: &net.TCPAddr{
				IP:   net.ParseIP("20.2.2.2"),
				Port: 2000,
			},
		}
		if _, err := header.WriteTo(conn); err != nil {
			cliResult <- err
			return
		}

		if _, err := conn.Write([]byte("test")); err != nil {
			cliResult <- err
			return
		}

		close(cliResult)
	}()

	conn, err := s.Listener.Accept()
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	defer conn.Close()

	recv := make([]byte, 1024)
	n, err := conn.Read(recv)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if string(recv[:n]) != "test" {
		t.Fatalf("expected \"test\", got \"%s\" %v", recv[:n], recv[:n])
	}
	err = <-cliResult
	if err != nil {
		t.Fatalf("client error: %v", err)
	}
}

func Test_MisconfiguredTLSServerRespondsWithUnderlyingError(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	s := NewTestTLSServer(l)
	s.Listener = &Listener{
		Listener: s.Listener,
		Policy: func(upstream net.Addr) (Policy, error) {
			return REQUIRE, nil
		},
	}
	defer s.Close()

	cliResult := make(chan error)
	go func() {
		// this is not a valid TLS connection, we are
		// connecting to the TLS endpoint via plain TCP.
		//
		// it's an example of a configuration error:
		// client: HTTP  -> PROXY
		// server: PROXY -> TLS -> HTTP
		//
		// we want to bubble up the underlying error,
		// in this case a tls handshake error, instead
		// of responding with a non-descript
		// > "Proxy protocol signature not present".

		conn, err := net.Dial("tcp", s.Addr())
		if err != nil {
			cliResult <- err
			return
		}
		defer conn.Close()

		// Write out the header!
		header := &Header{
			Version:           2,
			Command:           PROXY,
			TransportProtocol: TCPv4,
			SourceAddr: &net.TCPAddr{
				IP:   net.ParseIP("10.1.1.1"),
				Port: 1000,
			},
			DestinationAddr: &net.TCPAddr{
				IP:   net.ParseIP("20.2.2.2"),
				Port: 2000,
			},
		}
		if _, err := header.WriteTo(conn); err != nil {
			cliResult <- err
			return
		}

		if _, err := conn.Write([]byte("GET /foo/bar HTTP/1.1")); err != nil {
			cliResult <- err
			return
		}

		close(cliResult)
	}()

	conn, err := s.Listener.Accept()
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	defer conn.Close()

	recv := make([]byte, 1024)
	if _, err = conn.Read(recv); err.Error() != "tls: first record does not look like a TLS handshake" {
		t.Fatalf("expected tls handshake error, got %s", err)
	}
	err = <-cliResult
	if err != nil {
		t.Fatalf("client error: %v", err)
	}
}

type testConn struct {
	readFromCalledWith io.Reader
	reads              int
	net.Conn           // nil; crash on any unexpected use
}

func (c *testConn) ReadFrom(r io.Reader) (int64, error) {
	c.readFromCalledWith = r
	b, err := io.ReadAll(r)
	return int64(len(b)), err
}

func (c *testConn) Write(p []byte) (int, error) {
	return len(p), nil
}

func (c *testConn) Read(p []byte) (int, error) {
	if c.reads == 0 {
		return 0, io.EOF
	}
	c.reads--
	return 1, nil
}

func TestCopyToWrappedConnection(t *testing.T) {
	innerConn := &testConn{}
	wrappedConn := NewConn(innerConn)
	dummySrc := &testConn{reads: 1}

	if _, err := io.Copy(wrappedConn, dummySrc); err != nil {
		t.Fatalf("err: %v", err)
	}
	if innerConn.readFromCalledWith != dummySrc {
		t.Error("Expected io.Copy to delegate to ReadFrom function of inner destination connection")
	}
}

func TestCopyFromWrappedConnection(t *testing.T) {
	wrappedConn := NewConn(&testConn{reads: 1})
	dummyDst := &testConn{}

	if _, err := io.Copy(dummyDst, wrappedConn); err != nil {
		t.Fatalf("err: %v", err)
	}
	if dummyDst.readFromCalledWith != wrappedConn.conn {
		t.Errorf("Expected io.Copy to pass inner source connection to ReadFrom method of destination")
	}
}

func TestCopyFromWrappedConnectionToWrappedConnection(t *testing.T) {
	innerConn1 := &testConn{reads: 1}
	wrappedConn1 := NewConn(innerConn1)
	innerConn2 := &testConn{}
	wrappedConn2 := NewConn(innerConn2)

	if _, err := io.Copy(wrappedConn1, wrappedConn2); err != nil {
		t.Fatalf("err: %v", err)
	}
	if innerConn1.readFromCalledWith != innerConn2 {
		t.Errorf("Expected io.Copy to pass inner source connection to ReadFrom of inner destination connection")
	}
}

func benchmarkTCPProxy(size int, b *testing.B) {
	// create and start the echo backend
	backend, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatalf("err: %v", err)
	}
	defer backend.Close()
	go func() {
		for {
			conn, err := backend.Accept()
			if err != nil {
				break
			}
			_, err = io.Copy(conn, conn)
			// Can't defer since we keep accepting on each for iteration.
			_ = conn.Close()
			if err != nil {
				panic(fmt.Sprintf("Failed to read entire payload: %v", err))
			}
		}
	}()

	// start the proxyprotocol enabled tcp proxy
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatalf("err: %v", err)
	}
	defer l.Close()
	pl := &Listener{Listener: l}
	go func() {
		for {
			conn, err := pl.Accept()
			if err != nil {
				break
			}
			bConn, err := net.Dial("tcp", backend.Addr().String())
			if err != nil {
				panic(fmt.Sprintf("failed to dial backend: %v", err))
			}
			go func() {
				_, err = io.Copy(bConn, conn)
				_ = bConn.(*net.TCPConn).CloseWrite()
				if err != nil {
					panic(fmt.Sprintf("Failed to proxy incoming data to backend: %v", err))
				}
			}()
			_, err = io.Copy(conn, bConn)
			if err != nil {
				panic(fmt.Sprintf("Failed to proxy data from backend: %v", err))
			}
			_ = conn.Close()
			_ = bConn.Close()
		}
	}()

	data := make([]byte, size)

	header := &Header{
		Version:           2,
		Command:           PROXY,
		TransportProtocol: TCPv4,
		SourceAddr: &net.TCPAddr{
			IP:   net.ParseIP("10.1.1.1"),
			Port: 1000,
		},
		DestinationAddr: &net.TCPAddr{
			IP:   net.ParseIP("20.2.2.2"),
			Port: 2000,
		},
	}

	// now for the actual benchmark
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		conn, err := net.Dial("tcp", pl.Addr().String())
		if err != nil {
			b.Fatalf("err: %v", err)
		}
		// Write out the header!
		if _, err := header.WriteTo(conn); err != nil {
			b.Fatalf("err: %v", err)
		}
		// send data
		go func() {
			_, err = conn.Write(data)
			_ = conn.(*net.TCPConn).CloseWrite()
			if err != nil {
				panic(fmt.Sprintf("Failed to write data: %v", err))
			}
		}()
		// receive data
		n, err := io.Copy(io.Discard, conn)
		if n != int64(len(data)) {
			b.Fatalf("Expected to receive %d bytes, got %d", len(data), n)
		}
		if err != nil {
			b.Fatalf("Failed to read data: %v", err)
		}
		conn.Close()
	}
}

func BenchmarkTCPProxy16KB(b *testing.B) {
	benchmarkTCPProxy(16*1024, b)
}

func BenchmarkTCPProxy32KB(b *testing.B) {
	benchmarkTCPProxy(32*1024, b)
}

func BenchmarkTCPProxy64KB(b *testing.B) {
	benchmarkTCPProxy(64*1024, b)
}

func BenchmarkTCPProxy128KB(b *testing.B) {
	benchmarkTCPProxy(128*1024, b)
}

func BenchmarkTCPProxy256KB(b *testing.B) {
	benchmarkTCPProxy(256*1024, b)
}

func BenchmarkTCPProxy512KB(b *testing.B) {
	benchmarkTCPProxy(512*1024, b)
}

func BenchmarkTCPProxy1024KB(b *testing.B) {
	benchmarkTCPProxy(1024*1024, b)
}

func BenchmarkTCPProxy2048KB(b *testing.B) {
	benchmarkTCPProxy(2048*1024, b)
}

// copied from src/net/http/internal/testcert.go

// Copyright 2015 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// LocalhostCert is a PEM-encoded TLS cert with SAN IPs
// "127.0.0.1" and "[::1]", expiring at Jan 29 16:00:00 2084 GMT.
// generated from src/crypto/tls:
// go run generate_cert.go  --rsa-bits 1024 --host 127.0.0.1,::1,example.com --ca --start-date "Jan 1 00:00:00 1970" --duration=1000000h
var LocalhostCert = []byte(`-----BEGIN CERTIFICATE-----
MIICEzCCAXygAwIBAgIQMIMChMLGrR+QvmQvpwAU6zANBgkqhkiG9w0BAQsFADAS
MRAwDgYDVQQKEwdBY21lIENvMCAXDTcwMDEwMTAwMDAwMFoYDzIwODQwMTI5MTYw
MDAwWjASMRAwDgYDVQQKEwdBY21lIENvMIGfMA0GCSqGSIb3DQEBAQUAA4GNADCB
iQKBgQDuLnQAI3mDgey3VBzWnB2L39JUU4txjeVE6myuDqkM/uGlfjb9SjY1bIw4
iA5sBBZzHi3z0h1YV8QPuxEbi4nW91IJm2gsvvZhIrCHS3l6afab4pZBl2+XsDul
rKBxKKtD1rGxlG4LjncdabFn9gvLZad2bSysqz/qTAUStTvqJQIDAQABo2gwZjAO
BgNVHQ8BAf8EBAMCAqQwEwYDVR0lBAwwCgYIKwYBBQUHAwEwDwYDVR0TAQH/BAUw
AwEB/zAuBgNVHREEJzAlggtleGFtcGxlLmNvbYcEfwAAAYcQAAAAAAAAAAAAAAAA
AAAAATANBgkqhkiG9w0BAQsFAAOBgQCEcetwO59EWk7WiJsG4x8SY+UIAA+flUI9
tyC4lNhbcF2Idq9greZwbYCqTTTr2XiRNSMLCOjKyI7ukPoPjo16ocHj+P3vZGfs
h1fIw3cSS2OolhloGw/XM6RWPWtPAlGykKLciQrBru5NAPvCMsb/I1DAceTiotQM
fblo6RBxUQ==
-----END CERTIFICATE-----`)

// LocalhostKey is the private key for localhostCert.
var LocalhostKey = []byte(`-----BEGIN RSA PRIVATE KEY-----
MIICXgIBAAKBgQDuLnQAI3mDgey3VBzWnB2L39JUU4txjeVE6myuDqkM/uGlfjb9
SjY1bIw4iA5sBBZzHi3z0h1YV8QPuxEbi4nW91IJm2gsvvZhIrCHS3l6afab4pZB
l2+XsDulrKBxKKtD1rGxlG4LjncdabFn9gvLZad2bSysqz/qTAUStTvqJQIDAQAB
AoGAGRzwwir7XvBOAy5tM/uV6e+Zf6anZzus1s1Y1ClbjbE6HXbnWWF/wbZGOpet
3Zm4vD6MXc7jpTLryzTQIvVdfQbRc6+MUVeLKwZatTXtdZrhu+Jk7hx0nTPy8Jcb
uJqFk541aEw+mMogY/xEcfbWd6IOkp+4xqjlFLBEDytgbIECQQDvH/E6nk+hgN4H
qzzVtxxr397vWrjrIgPbJpQvBsafG7b0dA4AFjwVbFLmQcj2PprIMmPcQrooz8vp
jy4SHEg1AkEA/v13/5M47K9vCxmb8QeD/asydfsgS5TeuNi8DoUBEmiSJwma7FXY
fFUtxuvL7XvjwjN5B30pNEbc6Iuyt7y4MQJBAIt21su4b3sjXNueLKH85Q+phy2U
fQtuUE9txblTu14q3N7gHRZB4ZMhFYyDy8CKrN2cPg/Fvyt0Xlp/DoCzjA0CQQDU
y2ptGsuSmgUtWj3NM9xuwYPm+Z/F84K6+ARYiZ6PYj013sovGKUFfYAqVXVlxtIX
qyUBnu3X9ps8ZfjLZO7BAkEAlT4R5Yl6cGhaJQYZHOde3JEMhNRcVFMO8dJDaFeo
f9Oeos0UUothgiDktdQHxdNEwLjQf7lJJBzV+5OtwswCWA==
-----END RSA PRIVATE KEY-----`)
