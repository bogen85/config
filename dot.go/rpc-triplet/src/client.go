//go:build linux

package main

import (
	"fmt"
	"log"
	"net"
	"net/rpc/jsonrpc"
	"os"
	//"string"
)

func main() {
	conn, err := net.Dial("tcp", "127.0.0.1:5556") // broker's local port
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()

	cli := jsonrpc.NewClient(conn) // <-- important: pass raw net.Conn

	req := map[string]any{
		//"cmd":  "status",
		//"args": map[string]any{"verbose": true, "argv": fmt.Sprintf("%v", os.Args)},
		"args": map[string]any{"argv": fmt.Sprintf("%v", os.Args)},
	}
	var resp map[string]any
	if err := cli.Call("CLIbroker.Handle", req, &resp); err != nil {
		log.Fatal("call:", err)
	}
	fmt.Printf("response: %#v\n", resp)
}
