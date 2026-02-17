// Copyright 2017 The Gorilla WebSocket Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package websocket implements the WebSocket protocol defined in RFC 6455.
// This file contains proxy-related functionality for WebSocket connections.
package websocket

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"

	"golang.org/x/net/proxy"
)

// netDialerFunc is a function type that can dial network connections with context.
type netDialerFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// Dial implements the net.Dialer interface by calling the function with a background context.
func (fn netDialerFunc) Dial(network, addr string) (net.Conn, error) {
	return fn(context.Background(), network, addr)
}

// DialContext implements the proxy.ContextDialer interface.
func (fn netDialerFunc) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	return fn(ctx, network, addr)
}

// proxyFromURL creates a dialer that connects through the specified proxy.
// It supports HTTP proxies and any proxy type supported by golang.org/x/net/proxy.
func proxyFromURL(proxyURL *url.URL, forwardDial netDialerFunc) (netDialerFunc, error) {
	if proxyURL.Scheme == "http" {
		return (&httpProxyDialer{proxyURL: proxyURL, forwardDial: forwardDial}).DialContext, nil
	}

	// Handle non-HTTP proxies using the golang.org/x/net/proxy package
	dialer, err := proxy.FromURL(proxyURL, forwardDial)
	if err != nil {
		return nil, err
	}

	// If the dialer supports context, use its DialContext method
	if d, ok := dialer.(proxy.ContextDialer); ok {
		return d.DialContext, nil
	}

	// Otherwise, wrap the Dial method to accept a context
	return func(ctx context.Context, net, addr string) (net.Conn, error) {
		return dialer.Dial(net, addr)
	}, nil
}

// httpProxyDialer implements a dialer that connects through an HTTP proxy.
type httpProxyDialer struct {
	proxyURL    *url.URL
	forwardDial netDialerFunc
}

// DialContext establishes a connection to the address through the HTTP proxy.
func (hpd *httpProxyDialer) DialContext(ctx context.Context, network string, addr string) (net.Conn, error) {
	// Connect to the proxy server
	conn, err := hpd.connectToProxy(ctx, network)
	if err != nil {
		return nil, err
	}

	// Create and send the CONNECT request
	connectReq, err := hpd.createConnectRequest(addr)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}

	if err := connectReq.Write(conn); err != nil {
		_ = conn.Close()
		return nil, err
	}

	// Process the response from the proxy
	if err := hpd.processProxyResponse(conn, connectReq, addr); err != nil {
		_ = conn.Close()
		return nil, err
	}

	return conn, nil
}

// connectToProxy establishes a connection to the proxy server.
func (hpd *httpProxyDialer) connectToProxy(ctx context.Context, network string) (net.Conn, error) {
	hostPort, _ := hostPortNoPort(hpd.proxyURL)
	return hpd.forwardDial(ctx, network, hostPort)
}

// createConnectRequest creates an HTTP CONNECT request for the target address.
func (hpd *httpProxyDialer) createConnectRequest(addr string) (*http.Request, error) {
	connectHeader := make(http.Header)

	// Add proxy authentication if credentials are provided
	if user := hpd.proxyURL.User; user != nil {
		hpd.addProxyAuth(connectHeader, user)
	}

	return &http.Request{
		Method: http.MethodConnect,
		URL:    &url.URL{Opaque: addr},
		Host:   addr,
		Header: connectHeader,
	}, nil
}

// addProxyAuth adds the Proxy-Authorization header with Basic authentication.
func (hpd *httpProxyDialer) addProxyAuth(header http.Header, user *url.Userinfo) {
	proxyUser := user.Username()
	if proxyPassword, passwordSet := user.Password(); passwordSet {
		credential := base64.StdEncoding.EncodeToString([]byte(proxyUser + ":" + proxyPassword))
		header.Set("Proxy-Authorization", "Basic "+credential)
	}
}

// processProxyResponse reads and processes the HTTP response from the proxy.
func (hpd *httpProxyDialer) processProxyResponse(conn net.Conn, connectReq *http.Request, addr string) error {
	// Read response using a buffered reader
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, connectReq)
	if err != nil {
		return err
	}

	// Clean up the response to prevent resource leaks
	hpd.cleanupResponse(br, resp)

	// Check if the connection was established successfully
	if resp.StatusCode != http.StatusOK {
		return hpd.handleFailedConnection(resp)
	}

	return nil
}

// cleanupResponse properly cleans up the HTTP response to prevent resource leaks.
// Close the response body to silence false positives from linters. Reset
// the buffered reader first to ensure that Close() does not read from
// conn.
// Note: Applications must call resp.Body.Close() on a response returned
// http.ReadResponse to inspect trailers or read another response from the
// buffered reader. The call to resp.Body.Close() does not release
// resources.
func (hpd *httpProxyDialer) cleanupResponse(br *bufio.Reader, resp *http.Response) {
	// Reset the buffered reader to ensure that Close() does not read from conn
	br.Reset(bytes.NewReader(nil))

	// Close the response body to silence false positives from linters
	// Note: This doesn't actually release resources, but is required for proper HTTP handling
	_ = resp.Body.Close()
}

// handleFailedConnection processes a failed connection attempt and returns an appropriate error.
func (hpd *httpProxyDialer) handleFailedConnection(resp *http.Response) error {
	// Extract the error message from the status
	f := strings.SplitN(resp.Status, " ", 2)
	if len(f) < 2 {
		return errors.New("unknown error from proxy")
	}
	return errors.New(f[1])
}
