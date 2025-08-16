// broker.go

package main

import (
	"log"
	"net"
	"sync"

	"net/rpc"
	"net/rpc/jsonrpc"
)

type forwarder struct {
	mu     sync.RWMutex
	up     *rpc.Client // JSON-RPC client over the current uplink conn
	uplink string
	client string
}

func (f *forwarder) setUp(c *rpc.Client) {
	f.mu.Lock()
	f.up = c
	f.mu.Unlock()
}

func (f *forwarder) call(method string, in any, out any) error {
	f.mu.RLock()
	up := f.up
	f.mu.RUnlock()
	if up == nil {
		return rpc.ErrShutdown
	}
	// If the uplink is dead, the call will error; drop the client so future calls
	// see ErrShutdown until the server side reconnects.
	if err := up.Call(method, in, out); err != nil {
		f.setUp(nil)
		return err
	}
	return nil
}

// RPC exposes the same method locally; it forwards to server.
type RPC struct{ f *forwarder }

func (r RPC) Handle(args map[string]any, reply *map[string]any) error {
	var resp map[string]any
	if err := r.f.call("CLI.Handle", args, &resp); err != nil {
		return err
	}
	*reply = resp
	return nil
}

func acceptUplink(f *forwarder) {
	ln, err := net.Listen("tcp", f.uplink)
	if err != nil {
		log.Fatalf("uplink listen: %v", err)
	}
	log.Println("Broker: waiting for server uplink on", f.uplink)

	for {
		c, err := ln.Accept()
		if err != nil {
			log.Println("uplink accept:", err)
			continue
		}
		log.Println("Broker: uplink connected")
		cli := jsonrpc.NewClient(c) // <-- pass raw net.Conn
		f.setUp(cli)

		// When this connection eventually dies, the next forwarder.call will
		// fail and clear f.up; server should reconnect to restore it.
	}
}

func serveClient(f *forwarder) {
	ln, err := net.Listen("tcp", f.client)
	if err != nil {
		log.Fatalf("client listen: %v", err)
	}
	log.Println("Broker: serving clients on", f.client)

	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Println("client accept:", err)
			continue
		}
		go func(c net.Conn) {
			defer c.Close()
			srv := rpc.NewServer()
			_ = srv.RegisterName("CLIbroker", &RPC{f: f})
			srv.ServeCodec(jsonrpc.NewServerCodec(c)) // <-- pass raw net.Conn
		}(conn)
	}
}

func main() {

	f := &forwarder{
		uplink: "127.0.0.1:5555", // Server dials this
		client: "127.0.0.1:5556", // Clients dial this
	}
	go acceptUplink(f)
	serveClient(f)
}
