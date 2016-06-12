package main

import (
    "flag"
    "strconv"

    "net"
    "bufio"
    "fmt"
    "strings"
    "time"

    "os"
    "os/signal"
    "syscall"
    "io"
    "io/ioutil"

    "encoding/binary"
    "encoding/hex"
    "bytes"

    "sync"
)

func check(e error, desc string) {
    if e != nil {
        panic(desc + ": " + e.Error())
    }
}

func assert(cond bool) {
    if cond == false {
        panic("assert failed")
    }
}

var filters = make([]Filter, 0)

func main() {
    // Parse command-line arguments.
    var listenPort int;
    flag.IntVar(&listenPort, "listen-port", 3500, "Listening TCP port of the mock")
    var listenCommandsSocket string;
    flag.StringVar(&listenCommandsSocket, "listen-commands-socket", "/tmp/tcp_mock.sock", "Listening UNIX socket for the mock commands")
    flag.Parse()

    // Create Iproto filters slice with initial size 0 and pass them to function, accepting control mock commands.
    // Mock commands can modify this slice of filters
    go listenMockCommands(listenCommandsSocket) // listen async in goroutine

    listenMockConnections(listenPort) // block on it
}

func listenMockConnections(listenPort int) {
    // Listen on TCP port.
    server, err := net.Listen("tcp", ":" + strconv.Itoa(listenPort))
    check(err, fmt.Sprintf("couldn't start listening on port %v", listenPort))

    defer server.Close() // Close the listener when the application closes.
    fmt.Printf("Listening on :%v\n", listenPort)

    acceptConnections(server) // blocking call
}

func closeListenerOnTermination(listener net.Listener) {
    sigc := make(chan os.Signal, 1)
    signal.Notify(sigc, os.Interrupt, os.Kill, syscall.SIGTERM)
    go func(c chan os.Signal) {
        // Wait for a SIGINT or SIGKILL:
        sig := <-c
        fmt.Printf("Caught signal %v: shutting down.\n", sig)

        // Stop listening (and unlink the socket if unix type).
        listener.Close()

        // And we're done.
        os.Exit(0)
    }(sigc)
}

func acceptConnections(server net.Listener) {
    for {
        // Accept and incoming connection.
        fmt.Println("accepting conn...")
        conn, err := server.Accept()
        fmt.Println("accepted conn")
        check(err, "Error accepting connection")

        // Handle connections in a new goroutine (async handling of request in coroutine).
        go handleConnection(conn)
    }
}

// Handles incoming requests.
func handleConnection(conn net.Conn) {
    conn.SetDeadline(time.Now().Add(1 * time.Second))
    defer conn.Close()

    filtersRwLock.RLock();
    defer filtersRwLock.RUnlock();
    c := FilterContext{conn: conn, ctx: make(map[string]interface{})}

    if len(filters) == 0 {
        fmt.Println("[conn=%p] no filters, close conn", &conn)
        return
    }

    for {
        c.req = make([]byte, 0) // initial request is empty
        filtersDone := 0
        var r FilterResult
        for _, f := range filters {
            r = f.processRequest(&c)
            filtersDone++
            if r != FrOk {
                break
            }
        }
        fmt.Printf("[conn=%p] req number: %v, req len: %v, filters applied: %v/%v, res: %v\n",
            &conn, c.reqsProcessedN, len(c.req), filtersDone, len(filters), r)
        if r == FrConnClose {
            fmt.Printf("[conn=%p] close conn\n", &conn)
            break
        }

        c.resp = make([]byte, 0) // initial response is empty
        filtersDone = 0
        for i := range filters {
            f := filters[len(filters) - 1 - i]
            r = f.processResponse(&c)
            filtersDone++
            if r != FrOk {
                break
            }
        }
        fmt.Printf("[conn=%p] req number: %v, resp len: %v, filters applied: %v/%v, res: %v\n",
            &conn, c.reqsProcessedN, len(c.resp), filtersDone, len(filters), r)
        if r == FrConnClose {
            fmt.Printf("[conn=%p] close conn\n", &conn)
            break
        }
        c.reqsProcessedN++
    }
}

func listenMockCommands(listenCommandsSocket string) {
    // Listen on unix socket.
    server, err := net.Listen("unix", listenCommandsSocket)
    check(err, fmt.Sprintf("couldn't start listening mock commands on UNIX socket %v", listenCommandsSocket))

    defer server.Close() // Close the listener when the application closes.
    closeListenerOnTermination(server) // Close the listener even if got SIGINT, etc

    fmt.Printf("Listening mock commands on %v\n", listenCommandsSocket)

    for {
        // Accept and incoming connection.
        conn, err := server.Accept()
        check(err, "Error accepting command connection")

        // Read command line.
        b := bufio.NewReader(conn)
        data, err := b.ReadBytes('\n')
        check(err, "Error reading from command connection")

        line := strings.TrimSpace(string(data[:]))

        fmt.Printf("Got mock command '%v'\n", line)

        s := strings.Split(line, " ")
        processMockCommand(s[0], s[1:])

        n, err := conn.Write([]byte("ok"))
        check(err, "can't write response to mock cmd")
        assert(n == len("ok"))
        conn.Close()
    }
}

func processMockCommand(cmd string, args []string) {
    filtersRwLock.Lock();
    defer filtersRwLock.Unlock();

    switch cmd{
    case "Filters.clear":
        filters = filters[0:0]
        fmt.Printf("cleared all filters\n")
    case "Filters.add":
        processMockCommandFilterAdd(args[0], args[1:])
    default:
        panic(fmt.Sprintf("unknown command %v", cmd))
    }
}

func processMockCommandFilterAdd(filterName string, args []string) {
    switch filterName{
    case "TcpRw":
        filters = append(filters, &TcpRwFilter{})
    case "FileDumper":
        f := makeFileDumperFilter(args[0])
        filters = append(filters, &f)
    case "ConstResponse":
        filters = append(filters, makeConstResponseFilter([]byte(args[0])))
    case "IprotoWrapper":
        filters = append(filters, &IprotoWrapperFilter{})
    default:
        panic(fmt.Sprintf("filter '%v' does't exist", filterName))
    }

    fmt.Printf("Created filter '%v'\n", filterName)
}

// =====================
// FILTERS
// =====================

type FilterContext struct {
    conn net.Conn
    req []byte
    resp []byte
    reqsProcessedN int

    // Context data of filters.
    ctx map[string]interface{}
}

type FilterResult int
const (
    FrOk = iota
    FrStopChain
    FrConnClose
)

type Filter interface {
    // Process full request data.
    processRequest(c *FilterContext) FilterResult

    // Process full response data.
    processResponse(c *FilterContext) FilterResult
}

type DummyFilter struct {}

func (f DummyFilter) processRequest(c *FilterContext) FilterResult { return FrOk }
func (f DummyFilter) processResponse(c *FilterContext) FilterResult { return FrOk }

var filtersRwLock = &sync.RWMutex{}

// =====================
// CONCRETE FILTERS
// =====================

// TcpRwFilter

type TcpRwFilter struct {}

func (f TcpRwFilter) processRequest(c *FilterContext) FilterResult {
    assert(c.reqsProcessedN == 0) // We should close conn in response handler.
    assert(len(c.req) == 0) // Should be first.
    req, err := ioutil.ReadAll(c.conn)
    fmt.Printf("read %v bytes until EOF\n", len(req))
    check(err, "can't read request data")
    c.req = req
    return FrOk
}

func (f TcpRwFilter) processResponse(c *FilterContext) FilterResult {
    if len(c.resp) == 0 {
        return FrConnClose // Stop processing.
    }

    n, err := c.conn.Write(c.resp)
    check(err, "can't write response")
    assert(n == len(c.resp))
    return FrConnClose // Stop processing.
}

// FileDumperFilter

type FileDumperFilter struct {
    DummyFilter
    path string
}

func makeFileDumperFilter(path string) FileDumperFilter { return FileDumperFilter{path: path} }

func (f FileDumperFilter) processRequest(c *FilterContext) FilterResult {
    var reqsDumpedN int
    if c.ctx["reqsDumpedN"] == nil {
        reqsDumpedN = 0
    } else {
        reqsDumpedN = c.ctx["reqsDumpedN"].(int)
    }
    path := fmt.Sprintf("%s.%d", f.path, reqsDumpedN)
    err := ioutil.WriteFile(path, c.req, 0644)
    check(err, "can't write to file " + path)
    fmt.Printf("Printed request #%v body of len %v into file '%v'\n", reqsDumpedN, len(c.req), path)
    reqsDumpedN++
    c.ctx["reqsDumpedN"] = reqsDumpedN
    return FrOk
}

// ConstResponseFilter

type ConstResponseFilter struct {
    DummyFilter
    resp []byte
}

func makeConstResponseFilter(encodedResp []byte) ConstResponseFilter {
    // Hex-decode resp: it's encoded to allow any character inside response.
    decodedResp := make([]byte, hex.DecodedLen(len(encodedResp)))
    _, err := hex.Decode(decodedResp, encodedResp)
    check(err, "can't hex-decode resp")

    return ConstResponseFilter{resp: decodedResp}
}

func (f ConstResponseFilter) processResponse(c *FilterContext) FilterResult {
    c.resp = f.resp
    return FrOk
}

// IprotoWrapperFilter

type IprotoHeader struct {
    svc uint32
    length uint32
    sync uint32
}

func (h IprotoHeader) String() string {
    return fmt.Sprintf("[svc=%v; len=%v; sync=%v]", h.svc, h.length, h.sync)
}

type IprotoWrapperFilter struct {}

func (f IprotoWrapperFilter) processRequest(c *FilterContext) FilterResult {
    assert(len(c.req) == 0) // Should be first.
    reqHdr, reqBody, err := unpackIprotoData(c.conn)
    if err != nil {
        return FrConnClose // Stop requests processing (EOF).
    }

    c.ctx["iprotoHdr"] = reqHdr
    if reqHdr.svc == 0xff00 { // Got iproto ping request.
        fmt.Println("got iproto PING, stop req chain")
        return FrStopChain
    }

    fmt.Printf("got iproto request with hdr=%v\n", reqHdr.String())
    c.req = reqBody // Unpack and remove iproto header.
    return FrOk
}

func (f IprotoWrapperFilter) processResponse(c *FilterContext) FilterResult {
    reqHdr := c.ctx["iprotoHdr"].(IprotoHeader)
    assert(reqHdr.svc != 0)
    if reqHdr.svc == 0xff00 { // Got iproto ping request.
        _, err := c.conn.Write(reqHdr.Bytes()) // Send iproto header back.
        check(err, "can't write ping response")
        fmt.Println("sent iproto PING response")
        return FrStopChain // Stop filters chain.
    }

    respHdr := IprotoHeader{svc: reqHdr.svc, length: uint32(len(c.resp)), sync: reqHdr.sync}
    fmt.Printf("Sending iproto response with header %v\n", respHdr.String())
    c.conn.Write(respHdr.Bytes())
    if len(c.resp) != 0 {
        c.conn.Write(c.resp)
    }
    c.resp = c.resp[0:0]

    return FrStopChain
}

func makeIprotoHeaderFromBytes(b []byte) IprotoHeader {
    hdr := IprotoHeader{}
    hdrBufReader := bytes.NewReader(b)
    binary.Read(hdrBufReader, binary.LittleEndian, &hdr.svc)
    binary.Read(hdrBufReader, binary.LittleEndian, &hdr.length)
    binary.Read(hdrBufReader, binary.LittleEndian, &hdr.sync)
    return hdr
}

func (h IprotoHeader) Bytes() []byte {
    buf := new(bytes.Buffer)
    binary.Write(buf, binary.LittleEndian, h.svc)
    binary.Write(buf, binary.LittleEndian, h.length)
    binary.Write(buf, binary.LittleEndian, h.sync)
    return buf.Bytes()
}

func unpackIprotoData(conn net.Conn) (IprotoHeader, []byte, error) {
    // Read iproto header.
    hdrBuf := make([]byte, 12)
    fmt.Sprintf("try to read 12 bytes...")
    hdrBufLen, err := conn.Read(hdrBuf)
    fmt.Sprintf("done to read 12 bytes...")
    if err != nil { // Maybe connection was closed.
        return IprotoHeader{}, nil, err
    }

    if hdrBufLen != 12 {
        panic(fmt.Sprintf("can't read iproto header: read %v bytes instead of 12", hdrBufLen));
    }

    fmt.Printf("hdr bin data: %v\n", hdrBuf)

    // Parse iproto header into struct IprotoHeader.
    hdr := makeIprotoHeaderFromBytes(hdrBuf)

    // Read iproto request body into the buffer.
    if hdr.length == 0 {
        return hdr, make([]byte, 0), nil
    }

    bodyBuf := make([]byte, hdr.length)
    n, err := io.ReadFull(conn, bodyBuf)
    check(err, "can't read iproto body")
    assert(n == int(hdr.length))

    return hdr, bodyBuf, nil
}
