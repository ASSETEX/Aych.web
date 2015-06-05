// Package httpunit tests compliance of net services.
package httpunit

import (
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Plans struct {
	Plans []*TestPlan `toml:"plan"`
	IPs   IPMap
}

type PlanResult struct {
	Plan   *TestPlan
	Case   *TestCase
	Result *TestResult
}

type Results []*PlanResult

// Test performs tests. Filter optionally specifies an IP filter to use. no10
// disallows 10.* addresses. It returns a channel where results will be sent
// when complete, and the total number of results to expect. The channel is
// closed once all results are completed.
func (ps *Plans) Test(filter string, no10 bool) (<-chan *PlanResult, int, error) {
	var wg sync.WaitGroup
	count := 0
	ch := make(chan *PlanResult)
	labels := make(map[string]bool)
	for _, p := range ps.Plans {
		if labels[p.Label] {
			return nil, 0, fmt.Errorf("duplicate label: %v", p.Label)
		}
		labels[p.Label] = true
		cs, err := p.Cases(filter, no10, ps.IPs)
		if err != nil {
			return nil, 0, fmt.Errorf("%v: %v", p.Label, err)
		}
		for _, c := range cs {
			wg.Add(1)
			count++
			go func(p *TestPlan, c *TestCase) {
				r := c.Test()
				ch <- &PlanResult{
					Plan:   p,
					Case:   c,
					Result: r,
				}
				wg.Done()
			}(p, c)
		}
	}
	go func() {
		wg.Wait()
		close(ch)
	}()
	return ch, count, nil
}

// IPMap is a map of regular expressions to replacements.
type IPMap map[string][]string

var reAdd = regexp.MustCompile(`\((\d+)\+(\d+)\)`)

// Expand expands s into IP addresses. A successful return will consist of
// valid IP addresses or "*". The following process is repeated until there
// are no changes. If an address is a valid IP address or "*", no further
// changes to it are done. If it contains the form "(x+y)", x and y are added
// and replaced. Otherwise a regular expression search and replace is done
// for each key of i with its values.
func (i IPMap) Expand(s string) ([]string, error) {
	ir := make(map[*regexp.Regexp][]string)
	for k, v := range i {
		r, err := regexp.Compile(k)
		if err != nil {
			return nil, fmt.Errorf("%v: %v", k, err)
		}
		ir[r] = v
	}
	addrs := []string{s}
	for {
		if len(addrs) > 2000 {
			return nil, fmt.Errorf("address limit reached: maybe you need ^ and $ around a regex?")
		}
		var next []string
		for _, a := range addrs {
			if a == "*" {
				next = append(next, a)
				continue
			}
			if ip := net.ParseIP(a); ip != nil {
				next = append(next, ip.String())
				continue
			}
			if reAdd.MatchString(a) {
				n := reAdd.ReplaceAllStringFunc(a, func(s string) string {
					m := reAdd.FindStringSubmatch(s)
					l, _ := strconv.Atoi(m[1])
					r, _ := strconv.Atoi(m[2])
					return strconv.Itoa(l + r)
				})
				if n != a {
					next = append(next, n)
					continue
				}
			}
			added := false
			for r, reps := range ir {
				for _, rep := range reps {
					b := r.ReplaceAllString(a, rep)
					if a != b {
						next = append(next, b)
						added = true
					}
				}
				if added {
					break
				}
			}
			if !added {
				return nil, fmt.Errorf("unused address: %v", a)
			}
		}
		match := strEqual(addrs, next)
		addrs = next
		if match {
			break
		}
	}
	for _, a := range addrs {
		if ip := net.ParseIP(a); ip == nil && a != "*" {
			return nil, fmt.Errorf("bad ip: %v", a)
		}
	}
	return addrs, nil
}

func strEqual(a, b []string) bool {
	sort.Strings(a)
	sort.Strings(b)
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// Timeout is the connection timeout.
var Timeout = time.Second * 3

// TestPlan describes a test and its permutations (IP addresses).
type TestPlan struct {
	Label string
	URL   string
	IPs   []string

	Code  int
	Text  string
	Regex string
}

// Cases computes the actual test cases from a test plan. filter and no10 are described in Plans.Test.
func (p *TestPlan) Cases(filter string, no10 bool, IPs IPMap) ([]*TestCase, error) {
	if p.Label == "" {
		return nil, fmt.Errorf("%v: label must not be empty", p.URL)
	}
	u, err := url.Parse(p.URL)
	if err != nil {
		return nil, err
	} else if u.Host == "" {
		return nil, fmt.Errorf("no host")
	}
	sp := strings.Split(u.Host, ":")
	if len(sp) > 2 {
		return nil, fmt.Errorf("bad host")
	}
	host := sp[0]
	var port string
	if len(sp) > 1 {
		port = sp[1]
	}
	code := p.Code
	switch u.Scheme {
	case "http", "https":
		if code == 0 {
			code = 200
		}
		if port == "" {
			switch u.Scheme {
			case "http":
				port = "80"
			case "https":
				port = "443"
			}
		}
	case "tcp", "tcp4", "tcp6", "udp", "udp4", "udp6", "ip", "ip4", "ip6":
		if u.Path != "" || u.RawQuery != "" {
			return nil, fmt.Errorf("path and query must be unspecified in %s", u.Scheme)
		}
		if len(p.IPs) > 0 {
			return nil, fmt.Errorf("IPs not allowed for %s", u.Scheme)
		}
		if p.Code != 0 || p.Text != "" || p.Regex != "" {
			return nil, fmt.Errorf("'Expected' specs not allowed for %s", u.Scheme)
		}
	default:
		log.Fatalf("unknown protocol %s in %s", u.Scheme, p.URL)
	}
	var cases []*TestCase
	var re *regexp.Regexp
	if p.Regex != "" {
		r, err := regexp.Compile(p.Regex)
		if err != nil {
			return nil, err
		}
		re = r
	}
	add := func(fromDNS bool, ips ...string) error {
		for _, ip := range ips {
			if no10 && strings.HasPrefix(ip, "10.") {
				continue
			}
			if filter != "" {
				if strings.HasSuffix(filter, ".") {
					if !strings.HasPrefix(ip, filter) {
						continue
					}
				} else if ip != filter {
					continue
				}
			}
			c := &TestCase{
				URL:         u,
				IP:          net.ParseIP(ip),
				Port:        port,
				Plan:        p,
				FromDNS:     fromDNS,
				ExpectCode:  code,
				ExpectText:  p.Text,
				ExpectRegex: re,
			}
			if c.IP == nil {
				return fmt.Errorf("invalid ip: %v", ip)
			}
			cases = append(cases, c)
		}
		return nil
	}

	ips := p.IPs
	if len(ips) == 0 {
		ips = []string{"*"}
	}

	for _, ip := range ips {
		exp, err := IPs.Expand(ip)
		if err != nil {
			return nil, err
		}
		for _, i := range exp {
			if i == "*" {
				addrs, err := net.LookupHost(host)
				if err != nil {
					return nil, err
				}
				if err := add(true, addrs...); err != nil {
					return nil, err
				}
			} else {
				if err := add(false, i); err != nil {
					return nil, err
				}
			}
		}
	}

	return cases, nil
}

type TestCase struct {
	URL  *url.URL
	IP   net.IP
	Port string

	Plan *TestPlan
	// FromDNS is true if IP was determined with a DNS lookup.
	FromDNS bool

	ExpectCode  int
	ExpectText  string
	ExpectRegex *regexp.Regexp
}

type TestResult struct {
	Result error
	Resp   *http.Response

	Connected   bool
	GotCode     bool
	GotText     bool
	GotRegex    bool
	InvalidCert bool
}

func (c *TestCase) addr() string {
	return net.JoinHostPort(c.IP.String(), c.Port)
}

// Test performs this test case.
func (c *TestCase) Test() *TestResult {
	switch c.URL.Scheme {
	case "http", "https":
		return c.testHTTP()
	default:
		return c.testConnect()
	}
}

func (c *TestCase) testConnect() (r *TestResult) {
	r = new(TestResult)
	conn, err := net.DialTimeout(c.URL.Scheme, c.addr(), Timeout)
	if err != nil {
		r.Result = err
		return
	}
	r.Connected = true
	conn.Close()
	return
}

func (c *TestCase) testHTTP() (r *TestResult) {
	r = new(TestResult)
	tr := &http.Transport{
		Dial: func(network, a string) (net.Conn, error) {
			conn, err := net.DialTimeout(network, c.addr(), Timeout)
			if err != nil {
				r.Connected = false
			}
			return conn, err
		},
		DisableKeepAlives: true,
	}
	req, err := http.NewRequest("GET", c.URL.String(), nil)
	if err != nil {
		r.Result = err
		return
	}
	time.AfterFunc(Timeout, func() {
		r.Connected = false
		tr.CancelRequest(req)
	})
	resp, err := tr.RoundTrip(req)
	if err != nil {
		if strings.HasPrefix(err.Error(), "x509") {
			r.InvalidCert = true
		}
		r.Result = err
		return
	}
	defer resp.Body.Close()
	r.Resp = resp
	if resp.StatusCode != c.ExpectCode {
		r.Result = fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	} else {
		r.GotCode = true
	}
	text, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		r.Result = err
		return
	}
	short := text
	if len(short) > 250 {
		short = short[:250]
	}
	if c.ExpectText != "" && !strings.Contains(string(text), c.ExpectText) {
		r.Result = fmt.Errorf("response does not contain text [%s]: %q", c.ExpectText, short)
	} else {
		r.GotText = true
	}
	if c.ExpectRegex != nil && !c.ExpectRegex.Match(text) {
		r.Result = fmt.Errorf("response does not match regex [%s]: %q", c.ExpectRegex, short)
	} else {
		r.GotRegex = true
	}
	return
}
