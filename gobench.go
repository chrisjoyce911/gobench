package main

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"math/rand"
	"net"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pborman/uuid"
	"github.com/valyala/fasthttp"
)

// Global variables
var (
	requests         int64
	period           int64
	clients          int
	url              string
	urlsFilePath     string
	keepAlive        bool
	postDataFilePath string
	writeTimeout     int
	readTimeout      int
	authHeader       string
	userAgent        string
	acceptEnc        string
	randomize        bool
	insecure         bool
	verbose          bool
	contentType      string
	uriSubstitution  bool
)

// Benchmark Client Configuration
type Configuration struct {
	urls            []string
	method          string
	postData        []byte
	requests        int64
	period          int64
	keepAlive       bool
	authHeader      string
	acceptEnc       string
	randomize       bool
	contentType     string
	uriSubstitution bool

	myClient fasthttp.Client
}

type Result struct {
	requests      int64
	success       int64
	networkFailed int64
	badFailed     int64
	elapse        []float64
}

var readThroughput int64
var writeThroughput int64

// connection
type MyConn struct {
	net.Conn
}

func (this *MyConn) Read(b []byte) (n int, err error) {
	len, err := this.Conn.Read(b)

	if err == nil {
		atomic.AddInt64(&readThroughput, int64(len))
	}

	return len, err
}

func (this *MyConn) Write(b []byte) (n int, err error) {
	len, err := this.Conn.Write(b)

	if err == nil {
		atomic.AddInt64(&writeThroughput, int64(len))
	}

	return len, err
}

func init() {
	flag.Int64Var(&requests, "r", -1, "Number of requests per client")
	flag.IntVar(&clients, "c", 100, "Number of concurrent clients")
	flag.StringVar(&url, "u", "", "URL")
	flag.StringVar(&urlsFilePath, "f", "", "URL's file path (line seperated)")
	flag.BoolVar(&keepAlive, "k", true, "Do HTTP keep-alive")
	flag.StringVar(&postDataFilePath, "d", "", "HTTP POST data file path")
	flag.Int64Var(&period, "t", -1, "Period of time (in seconds)")
	flag.IntVar(&writeTimeout, "tw", 5000, "Write timeout (in milliseconds)")
	flag.IntVar(&readTimeout, "tr", 5000, "Read timeout (in milliseconds)")
	flag.StringVar(&authHeader, "auth", "", "Authorization header")
	flag.StringVar(&userAgent, "agent", "", "User-Agent header")
	flag.StringVar(&acceptEnc, "accept", "", "Accept-Encoding header")
	flag.BoolVar(&randomize, "random", false, "Randomize URL order")
	flag.BoolVar(&insecure, "insecure", false, "Skip verifing SSL certificate")
	flag.BoolVar(&verbose, "v", false, "Show debug messages")
	flag.StringVar(&contentType, "ct", "", "Content type")
	flag.BoolVar(&uriSubstitution, "s", false, "Support <UUID> & <CID> substition in uri")
}

func printResults(results map[int]*Result, startTime time.Time) {
	var requests int64
	var success int64
	var networkFailed int64
	var badFailed int64

	f, err := os.Create("delay.txt")
	if err != nil {
		fmt.Println("open file failed")
		panic(err)
	}
	defer f.Close()

	for _, result := range results {
		requests += result.requests
		success += result.success
		networkFailed += result.networkFailed
		badFailed += result.badFailed
		for _, rtt := range result.elapse {
			fmt.Fprintf(f, "%f\n", rtt)
		}
	}

	elapsed := int64(time.Since(startTime).Seconds())

	if elapsed == 0 {
		elapsed = 1
	}

	fmt.Println()
	fmt.Printf("Requests:                       %10d hits\n", requests)
	fmt.Printf("Successful requests:            %10d hits\n", success)
	fmt.Printf("Network failed:                 %10d hits\n", networkFailed)
	fmt.Printf("Bad requests failed (!2xx):     %10d hits\n", badFailed)
	fmt.Printf("Successful requests rate:       %10d hits/sec\n", success/elapsed)
	fmt.Printf("Read throughput:                %10d bytes/sec\n", readThroughput/elapsed)
	fmt.Printf("Write throughput:               %10d bytes/sec\n", writeThroughput/elapsed)
	fmt.Printf("Test time:                      %10d sec\n", elapsed)
	fmt.Printf("Average request latency:              %4.2f msec\n", float64(elapsed)/float64(success)*1000)
}

func readLines(path string) (lines []string, err error) {

	var file *os.File
	var part []byte
	var prefix bool

	if file, err = os.Open(path); err != nil {
		return
	}
	defer file.Close()

	reader := bufio.NewReader(file)
	buffer := bytes.NewBuffer(make([]byte, 0))
	for {
		if part, prefix, err = reader.ReadLine(); err != nil {
			break
		}
		buffer.Write(part)
		if !prefix {
			lines = append(lines, buffer.String())
			buffer.Reset()
		}
	}
	if err == io.EOF {
		err = nil
	}
	return
}

func NewConfiguration() *Configuration {

	if urlsFilePath == "" && url == "" {
		flag.Usage()
		os.Exit(1)
	}

	if requests == -1 && period == -1 {
		fmt.Println("Requests or period must be provided")
		flag.Usage()
		os.Exit(1)
	}

	if requests != -1 && period != -1 {
		fmt.Println("Only one should be provided: [requests|period]")
		flag.Usage()
		os.Exit(1)
	}

	configuration := &Configuration{
		urls:            make([]string, 0),
		method:          "GET",
		postData:        nil,
		keepAlive:       keepAlive,
		requests:        int64((1 << 63) - 1),
		authHeader:      authHeader,
		acceptEnc:       acceptEnc,
		randomize:       randomize,
		uriSubstitution: uriSubstitution,
		contentType:     contentType}

	if period != -1 {
		configuration.period = period

		timeout := make(chan bool, 1)
		go func() {
			<-time.After(time.Duration(period) * time.Second)
			timeout <- true
		}()

		go func() {
			<-timeout
			if runtime.GOOS == "windows" {
				printResults(results, startTime)
				os.Exit(0)
			}
			pid := os.Getpid()
			proc, _ := os.FindProcess(pid)
			err := proc.Signal(os.Interrupt)
			if err != nil {
				log.Println(err)
				fmt.Println(err)
				return
			}
		}()
	}

	if requests != -1 {
		configuration.requests = requests
	}

	if urlsFilePath != "" {
		fileLines, err := readLines(urlsFilePath)

		if err != nil {
			log.Fatalf("Error in ioutil.ReadFile for file: %s Error: %s", urlsFilePath, err)
		}

		configuration.urls = fileLines
	}

	if url != "" {
		configuration.urls = append(configuration.urls, url)
	}

	if postDataFilePath != "" {
		configuration.method = "POST"

		data, err := ioutil.ReadFile(postDataFilePath)

		if err != nil {
			log.Fatalf("Error in ioutil.ReadFile for file path: %s Error:%s", postDataFilePath, err)
		}

		configuration.postData = data
	}

	configuration.myClient.ReadTimeout = time.Duration(readTimeout) * time.Millisecond
	configuration.myClient.WriteTimeout = time.Duration(writeTimeout) * time.Millisecond
	configuration.myClient.MaxConnsPerHost = clients
	configuration.myClient.Name = userAgent
	configuration.myClient.TLSConfig = &tls.Config{InsecureSkipVerify: insecure}

	configuration.myClient.Dial = MyDialer()

	return configuration
}

func MyDialer() func(address string) (conn net.Conn, err error) {
	return func(address string) (net.Conn, error) {
		conn, err := net.Dial("tcp", address)
		if err != nil {
			return nil, err
		}

		myConn := &MyConn{Conn: conn}

		return myConn, nil
	}
}

func uriReplacer(s string, id string) string {
	r := strings.NewReplacer("<UUID>", uuid.New(), "<CID>", id)
	return r.Replace(s)
}

func client(configuration *Configuration, result *Result, id string, done *sync.WaitGroup) {
	rand := rand.New(rand.NewSource(time.Now().UnixNano()))

	for result.requests < configuration.requests {
		var tmpUrls []string
		if configuration.randomize {
			tmpUrls = []string{configuration.urls[rand.Intn(len(configuration.urls))]}
		} else {
			tmpUrls = configuration.urls
		}
		for _, tmpUrl := range tmpUrls {

			req := fasthttp.AcquireRequest()

			req_start := time.Now()
			if configuration.uriSubstitution {
				req.SetRequestURI(uriReplacer(tmpUrl, id))
			} else {
				req.SetRequestURI(tmpUrl)
			}
			req.Header.SetMethodBytes([]byte(configuration.method))

			if len(configuration.acceptEnc) > 0 {
				req.Header.Set("Accept-Encoding", configuration.acceptEnc)
			}

			if len(configuration.contentType) > 0 {
				req.Header.Set("Content-Type", configuration.contentType)
			}
			req.SetBody(configuration.postData)

			resp := fasthttp.AcquireResponse()
			requestTimer := time.Now().UTC()
			err := configuration.myClient.Do(req, resp)
			if err != nil {
				fmt.Printf("%s\n", err)
			}
			statusCode := resp.StatusCode()
			if verbose {
				fmt.Printf("Got status code [%d] - Request took [%s]\n", statusCode, time.Since(requestTimer))
			}
			result.requests++
			if err != nil {
				fmt.Printf("Network error: %s\n", err)
				result.networkFailed++
				continue
			}
			if resp.StatusCode() != fasthttp.StatusOK {
				result.badFailed++
			} else {
				if verbose {
					fmt.Printf("Non-2xx Status Code returned: [%d]\n", statusCode)
				}
				result.success++
			}
			result.elapse = append(result.elapse, time.Since(req_start).Seconds())
		}
	}

	done.Done()
}

var results map[int]*Result = make(map[int]*Result)

var startTime time.Time

func main() {

	startTime = time.Now()
	var done sync.WaitGroup
	signalChannel := make(chan os.Signal, 2)
	signal.Notify(signalChannel, os.Interrupt)
	go func() {
		_ = <-signalChannel
		fmt.Println("in coroutine print results")
		printResults(results, startTime)
		fmt.Println("in coroutine print results done")
		os.Exit(0)
	}()

	flag.Parse()

	configuration := NewConfiguration()

	goMaxProcs := os.Getenv("GOMAXPROCS")

	if goMaxProcs == "" {
		runtime.GOMAXPROCS(runtime.NumCPU())
	}

	fmt.Printf("Dispatching %d clients\n", clients)

	done.Add(clients)
	for i := 0; i < clients; i++ {
		result := &Result{}
		results[i] = result
		go client(configuration, result, strconv.Itoa(i), &done)

	}
	fmt.Println("Waiting for results...")

	done.Wait()
	fmt.Println("wait is done")
	printResults(results, startTime)
}
