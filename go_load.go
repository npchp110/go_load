package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"
)

var (
	rate, Seconds int
	host, path    string
	paths         []string
	httpClient    *http.Client
)

const (
	MaxIdleConnections int = 1000
	RequestTimeout     int = 5
)

type V struct {
	N, Mean, M2 float64
	_N, _M2     float64
}

func (v *V) Add(x float64) {
	v.N += 1.0
	v._N = v._N*0.999 + 1.0
	delta := x - v.Mean
	v.Mean += delta / v.N
	t := delta * (x - v.Mean)
	v.M2 += t
	v._M2 = v._M2*0.999 + t
}

func (v *V) Result() float64 {
	return v._M2 / (v._N - 1)
}

type Hist struct {
	data map[int]int
	keys []int
}

func (h *Hist) Add(x int) {
	if _, ok := h.data[x]; !ok {
		h.keys = append(h.keys, x)
	}
	h.data[x]++
}
func (h *Hist) format(unit string) string {
	sort.Ints(h.keys)
	dist := ""
	for _, i := range h.keys {
		dist += fmt.Sprintf("%d%s:%d ", i, unit, h.data[i])
	}
	return strings.TrimSpace(dist)
}

type StatsItem struct {
	Latency    float64
	StatusCode int
}

type Stats struct {
	V          V
	Total      int
	Max        float64
	Min        float64
	MsHist     Hist
	StatusHist Hist
}

func (s *Stats) Init() {
	s.MsHist.data = make(map[int]int)
	s.StatusHist.data = make(map[int]int)
}

func (s *Stats) Add(x float64, status int) {
	s.Max = math.Max(x, s.Max)
	s.Min = math.Min(x, s.Min)
	s.V.Add(x)
	s.MsHist.Add(int(x/100)*100 + 100)
	s.StatusHist.Add(status)
}

func (s *Stats) printMs() {
	fmt.Println("\t" + s.MsHist.format("ms"))
}

func (stat *Stats) printStat() {
	fmt.Printf("sent %d/%d requests avg:%.2f v:%.2f max:%.2f status_code[%s]\n",
		int(stat.V.N), stat.Total, stat.V.Mean, stat.V.Result(), stat.Max, stat.StatusHist.format(""))
}

// createHTTPClient for connection re-use
func createHTTPClient() *http.Client {
	client := &http.Client{
		Transport: &http.Transport{
			MaxIdleConnsPerHost: MaxIdleConnections,
		},
	}

	return client
}

func init() {
	flag.IntVar(&rate, "rate", 10, "")
	flag.IntVar(&Seconds, "seconds", 10, "")
	flag.StringVar(&host, "host", "", "")
	flag.StringVar(&path, "path", "", "")
	httpClient = createHTTPClient()
}

func main() {
	// Create a client
	flag.Parse()
	if path != "" {
		paths = strings.Split(path, ":")
		if len(paths) != 2 {
			log.Fatal("path format: <raw>:<new>")
		}
	}

	fmt.Printf("Start requests[rate:%d, seconds:%d url_file:%s]\n", rate, Seconds, flag.Arg(0))
	fmt.Println("Press <d> To Show Letency Histgram")
	f, err := os.Open(flag.Arg(0))
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()

	buf := make(chan string, 1000)
	ctrlChan := make(chan string, 10)
	go func() {
		s := bufio.NewScanner(f)
		for s.Scan() {
			buf <- s.Text()
		}
		close(buf)
	}()

	go func() {
		// disable input buffering
		exec.Command("stty", "-F", "/dev/tty", "cbreak", "min", "1").Run()
		// do not display entered characters on the screen
		exec.Command("stty", "-F", "/dev/tty", "-echo").Run()
		b := make([]byte, 1)
		for {
			os.Stdin.Read(b)
			if "d" == string(b) {
				ctrlChan <- "p"
			}
		}
	}()
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	timeout := time.After(time.Second * time.Duration(Seconds))

	go func() {
		select {
		case <-timeout:
			ctrlChan <- "q"
		case <-sigChan:
			ctrlChan <- "q"
		}
	}()

	sendURL(buf, ctrlChan)

}

func sendURL(buf chan string, ctrlChan chan string) {
	stat := Stats{Min: math.MaxFloat64}
	stat.Init()
	statCh := make(chan StatsItem, 1000)
	tick := time.Tick(time.Second / time.Duration(rate))

	for {
		select {
		case <-tick:
			u, ok := <-buf
			if !ok {
				return
			}
			stat.Total++
			go func(r string) {
				if host != "" {
					u, err := url.Parse(r)
					if err != nil {
						fmt.Printf("parse url[%s] fail: %s\n", r, err.Error())
						return
					}
					u.Host = host
					if len(paths) == 2 {
						u.Path = strings.Replace(u.Path, paths[0], paths[1], 1)
					}
					r = u.String()
				}
				begin := time.Now()

				rs, err := httpClient.Get(r)
				if err != nil {
					fmt.Printf("send request fail: %s\n", err.Error())
					return
				}
				io.Copy(ioutil.Discard, rs.Body)
				rs.Body.Close()

				go func() {
					statCh <- StatsItem{float64(time.Now().Sub(begin) / time.Millisecond), rs.StatusCode}
				}()
			}(u)
		case x := <-statCh:
			stat.Add(x.Latency, x.StatusCode)
			if n := int(stat.V.N); n%1000 == 0 {
				stat.printStat()
			}
		case cmd := <-ctrlChan:
			switch cmd {
			case "q":
				stat.printStat()
				stat.printMs()
				return
			case "p":
				stat.printMs()
			}
		}
	}
}
