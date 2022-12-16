package main

import (
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
)

//*************************************************************************************************
//*************************************************************************************************

// if we only want to save data with a specific extension, we can specify it here or in the command line argument
var saveExtension string = ""

//*************************************************************************************************
//*************************************************************************************************

type fileData struct {
	fullURL  string
	data     []byte
	position int64
}

//*************************************************************************************************
//*************************************************************************************************

func ping(localAddress, remoteAddress string) bool {
	localAddress += ":0"
	// not an actual ping (ICMP echo), but good enough for now
	fmt.Println("'pinging' using local address", localAddress)

	lAddress, err := net.ResolveTCPAddr("tcp", localAddress)
	if err != nil {
		return false
	}

	rAddress, err := net.ResolveTCPAddr("tcp", remoteAddress)
	if err != nil {
		return false
	}

	conn, err := net.DialTCP("tcp", lAddress, rAddress)
	if err != nil {
		fmt.Println(err)
		return false
	}

	fmt.Println("connected using", conn.LocalAddr().String())
	conn.Close()

	return true
}

//*************************************************************************************************
//*************************************************************************************************

func getLocalIP() (string, error) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		fmt.Println("could not get host's addresses")
	}

	for i := 0; i < len(addrs); i++ {
		localAddr := addrs[i].String()
		if strings.HasPrefix(localAddr, "192.168.1.") {
			localAddr = strings.Split(localAddr, "/")[0]

			// make sure this address is usable (connected to the internet)
			if ping(localAddr, "www.google.com:443") {
				return localAddr, nil
			}
		}
	}

	return "", errors.New("could not find local address")
}

//*************************************************************************************************
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

	return serverName + ":" + port
}

//*************************************************************************************************
//*************************************************************************************************

func getRequestFromClient(client net.Conn, buffer []byte, timeout bool) ([]byte, error) {
	if timeout {
		client.SetReadDeadline(time.Now().Add(30 * time.Second))
	} else {
		client.SetReadDeadline(time.Time{})
	}

	size, err := client.Read(buffer)
	if err != nil {
		return nil, err
	}
	//fmt.Println("Read", size, "bytes:")
	return buffer[:size], nil
}

//*************************************************************************************************
//*************************************************************************************************

func sendOkToClient(client net.Conn) error {
	msg := "HTTP/1.1 200 OK\r\n\r\n"
	client.SetWriteDeadline(time.Now().Add(time.Minute))
	_, err := client.Write([]byte(msg))
	return err
}

//*************************************************************************************************
//*************************************************************************************************

func sendRequestToServer(server net.Conn, request []byte) error {
	server.SetWriteDeadline(time.Now().Add(time.Minute))
	_, err := server.Write(request)
	return err
}

//*************************************************************************************************
//*************************************************************************************************

func transferFromServerToClientAndSave(server net.Conn,
	client net.Conn, buffer []byte, serverAddress string, fullURL string, saveDataChan chan<- *fileData, fullURLChan <-chan string) error {

	// these defers will run when the function exits
	defer server.Close()
	defer client.Close()

	packetNum := 0

	for {
		server.SetReadDeadline(time.Now().Add(15 * time.Second))
		size, err := server.Read(buffer)
		if err != nil {
			fmt.Println("could not read data from", serverAddress, "packetNum:", packetNum)
			return err
		}

		packetNum++

		// if packetNum < 5 {
		// 	strResponse := string(buffer[:size])
		// 	strResponseSplit := strings.Split(strResponse, "\n")
		// 	for i := 0; i < len(strResponseSplit); i++ {
		// 		fmt.Println(strResponseSplit[i])
		// 	}
		// 	fmt.Printf("\nEnd of packet %v\n\n", packetNum)
		// }

		if fullURLChan != nil {
			newURL := ""

			// if fullURL is empty then wait with a 5 second timeout
			// otherwise if fullURL is a valid URL then read from the channel with no wait
			if len(fullURL) == 0 {
				//fmt.Println("waiting 5 seconds for URL")
				timeout := time.After(5 * time.Second)

				select {
				case newURL = <-fullURLChan:
				case <-timeout:
					fmt.Println("timeout waiting for URL")
				}
			} else {
				select {
				case newURL = <-fullURLChan:
				default:
				}
			}

			// only save it if it seems valid
			if len(newURL) > 0 {
				if len(saveExtension) > 0 && strings.Contains(newURL, saveExtension) {
					fmt.Println("newURL:", newURL)
				}
				fullURL = newURL
			}
		}

		// send the data (without the headers) to a go routine that saves it to a file
		// only send the data if it matches the required extension
		// TODO: need better method to determine when the headers are finished, could span more than 1 packet,
		// TODO: track it with bool headerFinished or do another read right away if \r\n\r\n is not present?
		if len(saveExtension) > 0 && strings.Contains(fullURL, saveExtension) {
			if packetNum == 1 {
				strResponse := string(buffer[:size])
				strResponseSplit := strings.SplitN(strResponse, "\r\n\r\n", 2)
				if len(strResponseSplit) > 1 {
					justTheDataBytes := []byte(strResponseSplit[1])

					var dataToSave fileData
					dataToSave.fullURL = fullURL
					dataToSave.data = make([]byte, len(justTheDataBytes))
					copy(dataToSave.data, justTheDataBytes)

					// Typical format is:
					// Content-Range: bytes 200114176-473388312/473388313
					// (?i) means case-insensitive. Group 1 (the parentheses) contains the number we want.
					re, _ := regexp.Compile(`(?i)Content-Range: bytes (\d+)-`)
					regexResult := re.FindStringSubmatch(strResponseSplit[0])
					if regexResult == nil {
						dataToSave.position = 0
					} else {
						dataToSave.position, _ = strconv.ParseInt(regexResult[1], 10, 64)
					}

					saveDataChan <- &dataToSave
				}
			} else {
				var dataToSave fileData
				dataToSave.fullURL = fullURL
				dataToSave.data = make([]byte, size)
				dataToSave.position = 0
				copy(dataToSave.data, buffer)
				saveDataChan <- &dataToSave
			}
		}

		client.SetWriteDeadline(time.Now().Add(time.Minute))
		_, err = client.Write(buffer[:size])
		if err != nil {
			fmt.Println("could not transfer data from", serverAddress, "to client", "packetNum:", packetNum)
			return err
		}
	}
}

//*************************************************************************************************
//*************************************************************************************************

func transferFromServerToClient(server net.Conn, client net.Conn, buffer []byte) error {
	// these defers will run when the function exits
	defer server.Close()
	defer client.Close()

	for {
		server.SetReadDeadline(time.Now().Add(30 * time.Second))
		size, err := server.Read(buffer)
		if err != nil {
			//fmt.Println("could not read data from", serverAddress)
			return err
		}

		client.SetWriteDeadline(time.Now().Add(time.Minute))
		_, err = client.Write(buffer[:size])
		if err != nil {
			//fmt.Println("could not transfer data from", serverAddress, "to client")
			return err
		}
	}
}

//*************************************************************************************************
//*************************************************************************************************

func transfer(src net.Conn, dst net.Conn, buffer []byte) error {

	for {
		src.SetReadDeadline(time.Now().Add(time.Minute))
		size, err := src.Read(buffer)
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
	}
}

//*************************************************************************************************
//*************************************************************************************************

func handleConnection(logData bool, client net.Conn, saveDataChan chan *fileData) {
	defer client.Close()
	buffer := make([]byte, 4096)

	request, err := getRequestFromClient(client, buffer, false)
	if err != nil {
		return
	}
	serverAddress := extractServerAndPort(request)
	//fmt.Println("will connect to server", serverAddress)

	fullURL := getFullURL(request)
	//fmt.Println("full URL:", fullURL)

	server, err := net.Dial("tcp", serverAddress)
	if err != nil {
		fmt.Println("could not connect to", serverAddress)
		return
	}

	defer server.Close()

	err = sendRequestToServer(server, request)
	if err != nil {
		fmt.Println("could not send request to server", serverAddress)
		return
	}

	if logData {
		transferFromServerToClientAndSave(server, client, buffer, serverAddress, fullURL, saveDataChan, nil)
	} else {
		transferFromServerToClient(server, client, buffer)
	}

	//fmt.Println("done with handleConnection for", client.RemoteAddr().String())
}

//*************************************************************************************************
//*************************************************************************************************

func handleTunnelConnection(client net.Conn) {
	defer client.Close()
	buffer := make([]byte, 4096)

	for {
		request, err := getRequestFromClient(client, buffer, false)
		if err != nil {
			break
		}
		serverAddress := extractServerAndPort(request)
		//fmt.Println("will connect to server", serverAddress)

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

	//fmt.Println("done with handleTunnelConnection for", client.RemoteAddr().String())
}

//*************************************************************************************************
//*************************************************************************************************

func handleInterceptConnection(client net.Conn, ca *x509.Certificate, caPEM []byte, caPrivKey *rsa.PrivateKey,
	newURLs chan<- string, saveDataChan chan *fileData) {

	defer client.Close()
	buffer := make([]byte, 4096)

	request, err := getRequestFromClient(client, buffer, true)
	if err != nil {
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
		//fmt.Println("could not upgrade to TLS", err)
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
		//fmt.Println("could not make TLS connection with server")
		return
	}
	defer tlsServerConn.Close()
	//fmt.Println("connected to the server", serverAddress)

	// start a go routine to transfer all response data from the server to the client
	buffer2 := make([]byte, 4096)
	fullURLChan := make(chan string, 2)
	go transferFromServerToClientAndSave(tlsServerConn, tlsClientConn, buffer2, serverAddress, "", saveDataChan, fullURLChan)

	for {
		// receive HTTPS request from client
		request, err = getRequestFromClient(tlsClientConn, buffer, false)
		if err != nil {
			break
		}

		// get the full URL and save it
		fullURL := getFullURL(request)
		//fmt.Println("full URL:", fullURL)
		if len(fullURL) > 0 {
			newURLs <- fullURL
		}

		// send the fullURL to the go routine transferFromServerToClientAndSave
		fullURLChan <- fullURL

		// send the request to the server
		err = sendRequestToServer(tlsServerConn, request)
		if err != nil {
			break
		}
		//fmt.Println("request sent to server")
	}

	//fmt.Println("done with handleInterceptConnection for", client.RemoteAddr().String())
}

//*************************************************************************************************
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
//*************************************************************************************************

func getFullURL(request []byte) string {
	strRequest := string(request)

	// if the first few chars are not ASCII, don't continue, it might be a binary upload of data to the server
	if !isASCII(strRequest[:4]) {
		return ""
	}

	lines := strings.Split(strRequest, "\n")

	// get the relative path
	firstLine := lines[0]
	firstLineSplit := strings.Split(firstLine, " ")
	if len(firstLineSplit) <= 1 {
		return ""
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
		//fmt.Println("***** could not find hostname for request:", strRequest)
		return ""
	}

	if strings.Contains(relPath, hostname) {
		return relPath
	} else {
		return hostname + relPath
	}
}

//*************************************************************************************************
//*************************************************************************************************

func saveDataToFile(saveDataChan <-chan *fileData) {
	// create a data folder if it doesn't exist
	_, err := os.Stat("data")
	if err != nil {
		os.Mkdir("data", 0666)
	}

	for {
		newData := <-saveDataChan

		// check if we should save it
		if len(saveExtension) > 0 {
			if !strings.Contains(newData.fullURL, saveExtension) {
				continue
			}
		}

		// figure out the file name we will use locally
		urlSplit := strings.Split(newData.fullURL, "/")
		fileName := "./data/" + urlSplit[len(urlSplit)-1]

		fd, err := os.OpenFile(fileName, os.O_CREATE|os.O_WRONLY, 0777)
		if err != nil {
			fmt.Println("could not open file:", fileName)
			continue
		}
		if newData.position > 0 {
			_, err = fd.Seek(newData.position, 0)
			if err != nil {
				fmt.Println("could not seek to position")
				continue
			}
		} else {
			info, _ := fd.Stat()
			_, err = fd.Seek(info.Size(), 0)
			if err != nil {
				fmt.Println("could not seek to end of file")
				continue
			}
		}

		_, err = fd.Write(newData.data)
		if err != nil {
			fmt.Println("failed writing to file")
		}
		fd.Close()
	}
}

//*************************************************************************************************
//*************************************************************************************************

func gatherURLs(newURLs <-chan string, requestURLs <-chan bool, sendURLs chan<- []string) {
	queue := make([]string, 0)
	// TODO: passing pointers to the strings would be more efficient, as well as using a real queue instead of a slice

	for {
		select {
		case url := <-newURLs:
			queue = append(queue, url)
			if len(queue) > 1000 {
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
//*************************************************************************************************

func serveURLs(addr string, requestURLs chan<- bool, recvURLs <-chan []string) {
	fmt.Printf("serving URLs on %v/urls.html\n", addr)
	fmt.Printf("serving data on %v/datalist.html\n", addr)

	http.HandleFunc("/urls.html", func(w http.ResponseWriter, req *http.Request) {
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

	http.HandleFunc("/datalist.html", func(w http.ResponseWriter, req *http.Request) {
		// start with the standard HTML info
		fmt.Fprint(w, "<!DOCTYPE html>\n<html>\n<head>\n<meta charset=\"utf-8\">\n<title>List of Data</title>\n</head>\n<body>\n")

		// create a link for each file
		dir, _ := os.ReadDir("./data")
		for i := 0; i < len(dir); i++ {
			fmt.Fprintf(w, "<a href=\"/data/%v\">%v</a><br>\n", dir[i].Name(), dir[i].Name())
		}

		// finish with the standard HTML info
		fmt.Fprint(w, "</body>\n</html>")
	})

	http.HandleFunc("/data/", func(w http.ResponseWriter, req *http.Request) {
		name := req.URL.Path[len("/data/"):]
		fmt.Println(name, "requested")
		http.ServeFile(w, req, "./data/"+name)
	})

	err := http.ListenAndServe(addr, nil)
	if err != nil {
		fmt.Println("Unable to server urls on address", addr)
		os.Exit(1)
	}
}

//*************************************************************************************************
//*************************************************************************************************

func main() {
	fmt.Println("Starting server")

	if len(os.Args) > 1 {
		saveExtension = os.Args[1]
		fmt.Println("will only save data with extension", saveExtension)
	} else {
		fmt.Println("will save all data")
	}

	//****************************************************

	localIP, err := getLocalIP()
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	//****************************************************

	// create a root CA if we don't already have one
	ca, caPEM, caPrivKey, err := loadCA()
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	//****************************************************

	newURLs := make(chan string, 10) // channel that all new URLs will be sent to for collection
	requestURLs := make(chan bool)   // a go routine can request all the current URLs by sending true to this channel and then...
	sendURLs := make(chan []string)  // ...a go routine will receive the requested URLs on this channel
	go gatherURLs(newURLs, requestURLs, sendURLs)
	go serveURLs(localIP+":2004", requestURLs, sendURLs)

	saveDataChan := make(chan *fileData, 20)
	go saveDataToFile(saveDataChan)

	//****************************************************

	httpListener, err := net.Listen("tcp", localIP+":2000")
	if err != nil {
		fmt.Println("Unable to bind to port")
		os.Exit(1)
	}
	fmt.Println("Listening on", localIP+":2000", "for HTTP proxy")

	go func() {
		for {
			conn, err := httpListener.Accept()
			//fmt.Println("Received HTTP connection from", conn.RemoteAddr())
			if err == nil {
				go handleConnection(false, conn, saveDataChan)
			}
		}
	}()

	//****************************************************

	httpListenerLogger, err := net.Listen("tcp", localIP+":2001")
	if err != nil {
		fmt.Println("Unable to bind to port")
		os.Exit(1)
	}
	fmt.Println("Listening on", localIP+":2001", "for HTTP proxy logger")

	go func() {
		for {
			conn, err := httpListenerLogger.Accept()
			//fmt.Println("Received HTTP connection from", conn.RemoteAddr())
			if err == nil {
				go handleConnection(true, conn, saveDataChan)
			}
		}
	}()

	//****************************************************

	httpsListener, err := net.Listen("tcp", localIP+":2002")
	if err != nil {
		fmt.Println("Unable to bind to port")
		os.Exit(1)
	}
	fmt.Println("Listening on", localIP+":2002", "for HTTPS tunnel")

	go func() {
		for {
			conn, err := httpsListener.Accept()
			//fmt.Println("Received HTTPS connection from", conn.RemoteAddr())
			if err == nil {
				go handleTunnelConnection(conn)
			}
		}
	}()

	//****************************************************

	interceptListener, err := net.Listen("tcp", localIP+":2003")
	if err != nil {
		fmt.Println("Unable to bind to port")
		os.Exit(1)
	}
	fmt.Println("Listening on", localIP+":2003", "for HTTPS intercept")

	for {
		conn, err := interceptListener.Accept()
		//fmt.Println("Received HTTPS connection from", conn.RemoteAddr())
		if err == nil {
			go handleInterceptConnection(conn, ca, caPEM, caPrivKey, newURLs, saveDataChan)
		}
	}
}
