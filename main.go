package main

import (
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
	"unicode"
)

//*************************************************************************************************

func extractServerAndPort(request []byte) string {
	strRequest := string(request)
	//fmt.Println(strRequest)

	lines := strings.Split(strRequest, "\n")
	firstLine := lines[0]
	full_url := strings.Split(firstLine, " ")[1]

	if full_url[:4] != "http" {
		full_url = "http://" + full_url
	}

	parsed, _ := url.Parse(full_url)
	serverName := parsed.Hostname()
	port := parsed.Port()
	if port == "" {
		if parsed.Scheme == "http" {
			port = "80"
		} else {
			port = "443"
		}
	}

	//secure := (port == "443") || (parsed.Scheme == "https")

	return serverName + ":" + port
}

//*************************************************************************************************

func getRequestFromClient(client net.Conn, buffer []byte) ([]byte, error) {
	//fmt.Println("waiting for request from the client")
	// TODO: what if the request is chunked?
	client.SetReadDeadline(time.Now().Add(30 * time.Second))
	size, err := client.Read(buffer)
	if err != nil {
		//fmt.Println("Client disconnected or error", err)
		return nil, err
	}
	//fmt.Println("Read", size, "bytes:")
	return buffer[:size], nil
}

//*************************************************************************************************

func sendOkToClient(client net.Conn) error {
	msg := "HTTP/1.1 200 OK\r\n\r\n"
	client.SetWriteDeadline(time.Now().Add(time.Minute))
	_, err := client.Write([]byte(msg))
	return err
}

//*************************************************************************************************

func sendRequestToServer(server net.Conn, request []byte) error {
	server.SetWriteDeadline(time.Now().Add(time.Minute))
	_, err := server.Write(request)
	return err
}

//*************************************************************************************************

func transferFromServerToClient(server net.Conn, client net.Conn, buffer []byte, serverAddress string) error {
	// TODO: add rate-limiting
	// TODO: if server uses transfer-encoding chunked then look for last chunk and then return
	for {
		server.SetReadDeadline(time.Now().Add(15 * time.Second))
		size, err := server.Read(buffer)
		if err == io.EOF {
			//fmt.Println("no more data from", serverAddress)
			return nil
		} else if errors.Is(err, os.ErrDeadlineExceeded) {
			return err
		} else if err != nil {
			fmt.Println("could not read data from", serverAddress)
			return err
		}

		client.SetWriteDeadline(time.Now().Add(time.Minute))
		_, err = client.Write(buffer[:size])
		if err != nil {
			fmt.Println("could not transfer data from", serverAddress, "to client")
			return err
		}
		//fmt.Println("transfered", n, "bytes from", serverAddress, "to client")
		//fmt.Println(string(buffer[:n]))
	}
}

//*************************************************************************************************

func transfer(src net.Conn, dst net.Conn, buffer []byte) error {
	// TODO: add rate-limiting
	for {
		src.SetReadDeadline(time.Now().Add(time.Minute))
		size, err := src.Read(buffer)
		if err == io.EOF {
			fmt.Println("no more data from", src.RemoteAddr().String())
			return nil
		}
		if err != nil {
			fmt.Println("could not read data from", src.RemoteAddr().String())
			return err
		}

		dst.SetWriteDeadline(time.Now().Add(time.Minute))
		_, err = dst.Write(buffer[:size])
		if err != nil {
			fmt.Println("could not transfer data to", dst.RemoteAddr().String())
			return err
		}
		fmt.Println("transfered", size, "bytes to", dst.RemoteAddr().String())
		//fmt.Println(string(buffer[:size]))
	}
}

//*************************************************************************************************

func handleConnection(client net.Conn) {
	defer client.Close()
	buffer := make([]byte, 4096)

	for {
		request, err := getRequestFromClient(client, buffer)
		if err != nil {
			break
		}
		serverAddress := extractServerAndPort(request)
		fmt.Println("will connect to server", serverAddress)

		// TODO: if it's the same server then no need to reconnect?
		server, err := net.Dial("tcp", serverAddress)
		if err != nil {
			fmt.Println("could not connect to", serverAddress)
			break
		}

		defer server.Close()

		err = sendRequestToServer(server, request)
		if err != nil {
			fmt.Println("could not send request to server", serverAddress)
			break
		}

		err = transferFromServerToClient(server, client, buffer, serverAddress)
		if err != nil {
			break
		}
	}

	fmt.Println("done with handleConnection for", client.RemoteAddr().String())
}

//*************************************************************************************************

func handleTunnelConnection(client net.Conn) {
	defer client.Close()
	buffer := make([]byte, 4096)

	for {
		request, err := getRequestFromClient(client, buffer)
		if err != nil {
			break
		}
		serverAddress := extractServerAndPort(request)
		fmt.Println("will connect to server", serverAddress)

		server, err := net.Dial("tcp", serverAddress)
		if err != nil {
			fmt.Println("could not connect to", serverAddress)
			break
		}
		defer server.Close()

		err = sendOkToClient(client)
		if err != nil {
			fmt.Println("could not send OK to client", err)
			break
		}

		go func() {
			buffer2 := make([]byte, 4096)
			for {
				err := transfer(client, server, buffer2)
				if err != nil {
					break
				}
			}
		}()

		for {
			err = transfer(server, client, buffer)
			if err != nil {
				break
			}
		}
	}

	fmt.Println("done with handleTunnelConnection for", client.RemoteAddr().String())
}

//*************************************************************************************************

func handleInterceptConnection(client net.Conn, ca *x509.Certificate, caPEM []byte, caPrivKey *rsa.PrivateKey, newURLs chan<- string) {
	defer client.Close()
	buffer := make([]byte, 4096)

	request, err := getRequestFromClient(client, buffer)
	if err != nil {
		fmt.Println("could not get initial request when intercepting client", client.RemoteAddr().String(), err)
		return
	}
	serverAddress := extractServerAndPort(request)
	//fmt.Println("will connect to server", serverAddress)

	err = sendOkToClient(client)
	if err != nil {
		fmt.Println("could not send OK to client", err)
		return
	}
	//fmt.Println("sent OK to client", client.RemoteAddr().String())

	// create fake cert to spoof the server
	serverCert, err := createFakeCert(serverAddress, ca, caPrivKey)
	if err != nil {
		fmt.Println("could not create fake cert", err)
		return
	}

	// do TLS handshake with the client
	tlsClientConn, err := upgradeToTLS(client, serverCert)
	if err != nil {
		fmt.Println("could not upgrade to TLS", err)
		return
	}
	//fmt.Println("upgraded to TLS")

	// make TLS connection to the server
	tlsServerConn, err := tls.DialWithDialer(
		&net.Dialer{Timeout: 30 * time.Second},
		"tcp",
		serverAddress,
		&tls.Config{CurvePreferences: []tls.CurveID{tls.CurveP256}, MinVersion: tls.VersionTLS12},
	)
	if err != nil {
		fmt.Println("could not make TLS connection with server")
		return
	}
	defer tlsServerConn.Close()
	//fmt.Println("connected to the server", serverAddress)

	for {
		// receive HTTPS request from client
		request, err = getRequestFromClient(tlsClientConn, buffer)
		if err != nil {
			break
		}
		//strRequest := strings.Split(string(request), "\n")
		//fmt.Println(strRequest[0], "\n", strRequest[1])
		//fmt.Println(string(request))

		// get the full URL and save it
		saveURL(newURLs, request)

		// send the request to the server
		err = sendRequestToServer(tlsServerConn, request)
		if err != nil {
			break
		}
		//fmt.Println("request sent to server")

		// get response from the server
		err = transferFromServerToClient(tlsServerConn, tlsClientConn, buffer, serverAddress)
		if err != nil {
			break
		}
	}

	//fmt.Println("done with handleInterceptConnection for", client.RemoteAddr().String())
}

//*************************************************************************************************

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] > unicode.MaxASCII {
			return false
		}
	}
	return true
}

//*************************************************************************************************

func saveURL(newURLs chan<- string, request []byte) {
	strRequest := string(request)

	// if the first few chars are not ASCII, don't continue, it might be a binary upload of data to the server
	if !isASCII(strRequest[:4]) {
		return
	}

	lines := strings.Split(strRequest, "\n")

	// get the relative path
	firstLine := lines[0]
	firstLineSplit := strings.Split(firstLine, " ")
	if len(firstLineSplit) <= 1 {
		return
	}
	relPath := firstLineSplit[1]

	// get the root path (hostname)
	hostname := ""
	for i := 0; i < len(lines); i++ {

		if strings.HasPrefix(strings.ToLower(lines[i]), "host:") {
			hostname = strings.Split(lines[i], " ")[1]
			hostname = strings.TrimRight(hostname, "\r\n ")
		}
	}

	if hostname == "" {
		fmt.Println("***** could not find hostname for request:", strRequest)
	}

	// send the full URL to the go routine that's collecting them
	fullURL := hostname + relPath
	newURLs <- fullURL
}

//*************************************************************************************************

func gatherURLs(newURLs <-chan string, requestURLs <-chan bool, sendURLs chan<- []string) {
	queue := make([]string, 0)
	// TODO: passing pointers to the strings would be more efficient, as well as using a real queue instead of a slice

	for {
		select {
		case url := <-newURLs:
			queue = append(queue, url)
			if len(queue) > 100 {
				queue = queue[1:]
			}
		case <-requestURLs:
			queueCopy := make([]string, len(queue))
			copy(queueCopy, queue)
			sendURLs <- queueCopy
		}
	}
}

//*************************************************************************************************

func serveURLs(port string, requestURLs chan<- bool, recvURLs <-chan []string) {
	fmt.Printf("serving URLs on %v/urls\n", port)

	http.HandleFunc("/urls", func(w http.ResponseWriter, req *http.Request) {
		// send a message to the other go routine and wait for the response containing the URLs
		requestURLs <- true
		allURLs := <-recvURLs

		// start with the standard HTML info
		fmt.Fprint(w, "<!DOCTYPE html>\n<html>\n<head>\n<meta charset=\"utf-8\">\n<title>Captured URLs</title>\n</head>\n<body>\n")

		// send all the URLs that we captured
		for i := 0; i < len(allURLs); i++ {
			fmt.Fprintf(w, "<p>%v</p>\n", allURLs[i])
		}

		// finish with the standard HTML info
		fmt.Fprint(w, "</body>\n</html>")
	})

	err := http.ListenAndServe(port, nil)
	if err != nil {
		fmt.Println("Unable to server urls on port", port)
		os.Exit(1)
	}
}

//*************************************************************************************************

func main() {
	fmt.Println("Starting server")

	//****************************************************

	// create a root CA if we don't already have one
	ca, caPEM, caPrivKey, err := loadCA()
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	//****************************************************

	newURLs := make(chan string, 10)
	requestURLs := make(chan bool)
	sendURLs := make(chan []string)
	go gatherURLs(newURLs, requestURLs, sendURLs)
	go serveURLs(":2003", requestURLs, sendURLs)

	//****************************************************

	httpListener, err := net.Listen("tcp", ":2000")
	if err != nil {
		fmt.Println("Unable to bind to port")
		os.Exit(1)
	}
	fmt.Println("Listening on 0.0.0.0:2000 for HTTP")

	go func() {
		for {
			conn, err := httpListener.Accept()
			fmt.Println("Received HTTP connection from", conn.RemoteAddr())
			if err == nil {
				go handleConnection(conn)
			}
		}
	}()

	//****************************************************

	httpsListener, err := net.Listen("tcp", ":2001")
	if err != nil {
		fmt.Println("Unable to bind to port")
		os.Exit(1)
	}
	fmt.Println("Listening on 0.0.0.0:2001 for HTTPS")

	go func() {
		for {
			conn, err := httpsListener.Accept()
			fmt.Println("Received HTTPS connection from", conn.RemoteAddr())
			if err == nil {
				go handleTunnelConnection(conn)
			}
		}
	}()

	//****************************************************

	interceptListener, err := net.Listen("tcp", ":2002")
	if err != nil {
		fmt.Println("Unable to bind to port")
		os.Exit(1)
	}
	fmt.Println("Listening on 0.0.0.0:2002 for HTTPS intercept")

	for {
		conn, err := interceptListener.Accept()
		//fmt.Println("Received HTTPS connection from", conn.RemoteAddr())
		if err == nil {
			go handleInterceptConnection(conn, ca, caPEM, caPrivKey, newURLs)
		}
	}
}