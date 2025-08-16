//go:build windows

package main

import (
	"fmt"
	"log"
	"net"
	"net/rpc"
	"net/rpc/jsonrpc"
	"time"
)

type Server struct{ id int }

type CLI struct{ s *Server }

func (r CLI) Handle(args map[string]any, reply *map[string]any) error {
	fmt.Printf("%d: Got:\n%v\n", r.s.id, args)
	out := map[string]any{
		"ok":      true,
		"message": "handled by Windows",
		"echo":    args,
		"id":      fmt.Sprintf("%d", r.s.id),
	}
	*reply = out
	r.s.id++
	return nil
}

func serveOnConn(c net.Conn, s *Server) {
	defer c.Close()
	srv := rpc.NewServer()
	_ = srv.RegisterName("CLI", &CLI{s: s})
	// Use the raw connection; no bufio wrapper
	srv.ServeCodec(jsonrpc.NewServerCodec(c))
}

func main() {
	uplink := "127.0.0.1:5555" // broker in WSL is listening here
	s := &Server{id: 0}
	for {
		conn, err := net.Dial("tcp", uplink)
		if err != nil {
			log.Println("dial uplink:", err)
			time.Sleep(1 * time.Second)
			continue
		}
		log.Println("Windows server: uplink connected; serving JSON-RPC")
		serveOnConn(conn, s)
		log.Println("Windows server: uplink closed; reconnecting")
		time.Sleep(500 * time.Millisecond)
	}
}
