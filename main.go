package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

func must(err error) {
	if err != nil {
		panic(err)
	}
}

func tcpServer() {
	ln, err := net.Listen("tcp", ":6969")
	must(err)
	defer ln.Close()

	for {
		conn, err := ln.Accept()
		must(err)
		go handleClient(conn)

	}
}

type Server struct {
	Stdout         io.Reader
	Stdin          io.Writer
	Cmd            *exec.Cmd
	Root           string
	ServerId       int32
	InitResponse   map[string]interface{}
	CancelShutdown context.CancelFunc
}

func readMessage(r *bufio.Reader) (map[string]interface{}, error) {
	contentLength := -1
	for {
		header, _, err := r.ReadLine()
		if err != nil {
			return nil, err
		}
		headerString := string(header)
		if headerString == "" {
			break
		}
		headerSplit := strings.Split(headerString, ": ")
		if headerSplit[0] == "Content-Length" {
			contentLength, err = strconv.Atoi(headerSplit[1])
			if err != nil {
				return nil, err
			}
		}
	}

	if contentLength == -1 {
		return nil, errors.New("Content-Length not found")
	}

	contentBytes := make([]byte, contentLength)
	io.ReadFull(r, contentBytes)
	contentString := string(contentBytes)
	contentMap := make(map[string]interface{})
	err := json.Unmarshal([]byte(contentString), &contentMap)

	if err != nil {
		return nil, err
	}
	return contentMap, nil
}

func lspGetRoot(init map[string]interface{}) string {
	return init["params"].(map[string]interface{})["rootPath"].(string)
}

func sendMessage(w io.Writer, message map[string]interface{}) {
	messageBytes, err := json.Marshal(message)
	must(err)
	messageString := string(messageBytes)
	fmt.Fprintf(w, "Content-Length: %d\r\n\r\n%s", len(messageString), messageString)
}

func msgReader(r io.Reader, c chan map[string]interface{}) {
	reader := bufio.NewReader(r)
	for {
		message, err := readMessage(reader)
		if err != nil {
			break
		}
		c <- message
	}
}

func handleClient(client net.Conn) error {
	defer client.Close()

	clientMessages := make(chan map[string]interface{})
	go msgReader(client, clientMessages)

	initialMessage := <-clientMessages
	root := lspGetRoot(initialMessage)

	server, err := findFreeServerOrSpawnNew(root)
    if err != nil {
        return err
    }
	serverMessages := make(chan map[string]interface{})
	go msgReader(server.Stdout, serverMessages)

	if server.InitResponse == nil {
		sendMessage(server.Stdin, initialMessage)
		initialResponse := <-serverMessages
		sendMessage(client, initialResponse)
		server.InitResponse = initialResponse
	} else {
		sendMessage(client, server.InitResponse)
		server.CancelShutdown()
	}

	idMap := make(map[interface{}]interface{})
	nextRequestId := 0

	for {
		select {
		case message := <-clientMessages:
			if method, exists := message["method"]; exists && method == "shutdown" {
				response := map[string]interface{}{
					"jsonrpc": "2.0",
					"id":      message["id"],
					"result":  nil,
				}
				sendMessage(client, response)
				break
			}
			// check if the message is a request
			if _, exists := message["method"]; exists {
				id := message["id"]
				idMap[nextRequestId] = id
				message["id"] = nextRequestId
				nextRequestId += 1
			}

			sendMessage(server.Stdin, message)
		case message := <-serverMessages:
			// check if the message is a response
			_, ResultExists := message["result"]
			_, ErrorExists := message["error"]
			if ResultExists || ErrorExists {
				id, exists := idMap[message["id"]]
				if !exists {
					// log error
					log.Println("Error: server sent response with unknown request id")
				}
				delete(idMap, message["id"])
				message["id"] = id
			}
			sendMessage(client, message)
		}
	}

	ctx := context.Background()
	ctx, cancel := context.WithCancel(ctx)
	server.CancelShutdown = cancel
    FreeServers = append(FreeServers, server)

	timeout := 20 * time.Minute

	select {
	case <-time.After(timeout):
		log.Println("shutting down server due to inactivity")
		server.Cmd.Process.Kill()
        for i, s := range FreeServers {
            if s.ServerId == server.ServerId {
                FreeServers = append(FreeServers[:i], FreeServers[i+1:]...)
                break
            }
        }
	case <-ctx.Done():
		log.Println("shutdown cancelled")
	}
	return nil
}

func findFreeServerOrSpawnNew(root string) (Server, error) {
	idx := -1
	for serverId, server := range FreeServers {
		if server.Root == root {
			idx = serverId
			break
		}
	}
	if idx == -1 {
		return spawnServer(root)
	} else {
		server := FreeServers[idx]
		FreeServers = append(FreeServers[:idx], FreeServers[idx+1:]...)
		return server, nil
	}
}

var nextServerId int32 = 0

func spawnServer(root string) (Server, error) {
	cmd := exec.Command("rust-analyzer")
    stdin, err := cmd.StdinPipe()
    if err != nil {
        return Server{}, err
    }
    stdout, err := cmd.StdoutPipe()
    if err != nil {
        return Server{}, err
    }
	cmd.Start()

	return Server{
		Stdout:   stdout,
		Stdin:    stdin,
		Cmd:      cmd,
		Root:     root,
		ServerId: atomic.AddInt32(&nextServerId, 1),
	}, nil
}

var FreeServers []Server = []Server{}

func main() {
	tcpServer()
}
