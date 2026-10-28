package fbtenant

import (
	"encoding/json"
	"fmt"
	"net"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"time"
)

// setElem is one element of an nftables timeout set as seen from a dump.
type setElem struct {
	// Remaining is the time left before the element expires.
	Remaining time.Duration
	// Timeout is the timeout the element was added with.
	Timeout time.Duration
}

// NFTOps isolates all nftables access so the mirror logic can be tested on
// mocks (CONTRACTS §3).
type NFTOps interface {
	// ListSet dumps a set: ip -> {remaining, timeout}.
	ListSet(table, set string) (map[string]setElem, error)
	// AddElements adds/updates elements in one transaction; ip -> timeout.
	AddElements(table, set string, elems map[string]time.Duration) error
	// DeleteElements removes elements in one transaction.
	DeleteElements(table, set string, ips []string) error
}

var nameRe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func validName(s string) bool { return nameRe.MatchString(s) }

func validIPv4(s string) bool {
	ip := net.ParseIP(s)
	return ip != nil && ip.To4() != nil
}

func validateSetArgs(table, set string) error {
	if !validName(table) {
		return fmt.Errorf("bad nft table name %q", table)
	}
	if !validName(set) {
		return fmt.Errorf("bad nft set name %q", set)
	}
	return nil
}

// nftOps is the default NFTOps implementation backed by the nft binary.
type nftOps struct {
	binary string // defaults to "nft"
}

func newNFTOps() *nftOps { return &nftOps{binary: "nft"} }

func (o *nftOps) run(args ...string) ([]byte, error) {
	bin := o.binary
	if bin == "" {
		bin = "nft"
	}
	out, err := exec.Command(bin, args...).CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("nft %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

// nftDump is the JSON shape of `nft -j list set inet <table> <set>`.
type nftDump struct {
	Nftables []struct {
		Set *struct {
			Name string            `json:"name"`
			Elem []json.RawMessage `json:"elem"`
		} `json:"set"`
	} `json:"nftables"`
}

// nftElemObj covers both element encodings:
// "1.2.3.4" and {"elem": {"val": "1.2.3.4", "timeout": 300, "expiration": 120}}
// as well as the flat {"val": ...} variant.
type nftElemObj struct {
	Elem *struct {
		Val        string   `json:"val"`
		Timeout    *float64 `json:"timeout"`
		Expiration *float64 `json:"expiration"`
	} `json:"elem"`
	Val        string   `json:"val"`
	Timeout    *float64 `json:"timeout"`
	Expiration *float64 `json:"expiration"`
}

func (o *nftOps) ListSet(table, set string) (map[string]setElem, error) {
	if err := validateSetArgs(table, set); err != nil {
		return nil, err
	}
	out, err := o.run("-j", "list", "set", "inet", table, set)
	if err != nil {
		return nil, err
	}
	var dump nftDump
	if err := json.Unmarshal(out, &dump); err != nil {
		return nil, fmt.Errorf("parse nft dump for %s/%s: %w", table, set, err)
	}
	res := map[string]setElem{}
	for _, obj := range dump.Nftables {
		if obj.Set == nil || obj.Set.Name != set {
			continue
		}
		for _, raw := range obj.Set.Elem {
			ip, elem, ok := parseNftElem(raw)
			if !ok {
				continue
			}
			res[ip] = elem
		}
	}
	return res, nil
}

func parseNftElem(raw json.RawMessage) (string, setElem, bool) {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if validIPv4(s) {
			return s, setElem{}, true
		}
		return "", setElem{}, false
	}
	var obj nftElemObj
	if err := json.Unmarshal(raw, &obj); err != nil {
		return "", setElem{}, false
	}
	val, timeout, expiration := obj.Val, obj.Timeout, obj.Expiration
	if obj.Elem != nil {
		val, timeout, expiration = obj.Elem.Val, obj.Elem.Timeout, obj.Elem.Expiration
	}
	if !validIPv4(val) {
		return "", setElem{}, false
	}
	elem := setElem{}
	if timeout != nil && *timeout > 0 {
		elem.Timeout = time.Duration(*timeout) * time.Second
	}
	if expiration != nil && *expiration > 0 {
		elem.Remaining = time.Duration(*expiration) * time.Second
	} else {
		elem.Remaining = elem.Timeout
	}
	return val, elem, true
}

func (o *nftOps) AddElements(table, set string, elems map[string]time.Duration) error {
	if err := validateSetArgs(table, set); err != nil {
		return err
	}
	if len(elems) == 0 {
		return nil
	}
	ips := make([]string, 0, len(elems))
	for ip := range elems {
		if !validIPv4(ip) {
			return fmt.Errorf("add element: bad ip %q", ip)
		}
		if elems[ip] <= 0 {
			return fmt.Errorf("add element %q: bad timeout %v", ip, elems[ip])
		}
		ips = append(ips, ip)
	}
	sort.Strings(ips)
	parts := make([]string, 0, len(ips))
	for _, ip := range ips {
		parts = append(parts, fmt.Sprintf("%s timeout %ds", ip, int(elems[ip]/time.Second)))
	}
	payload := "{ " + strings.Join(parts, ", ") + " }"
	_, err := o.run("add", "element", "inet", table, set, payload)
	return err
}

func (o *nftOps) DeleteElements(table, set string, ips []string) error {
	if err := validateSetArgs(table, set); err != nil {
		return err
	}
	if len(ips) == 0 {
		return nil
	}
	clean := make([]string, 0, len(ips))
	for _, ip := range ips {
		if !validIPv4(ip) {
			return fmt.Errorf("delete element: bad ip %q", ip)
		}
		clean = append(clean, ip)
	}
	sort.Strings(clean)
	payload := "{ " + strings.Join(clean, ", ") + " }"
	_, err := o.run("delete", "element", "inet", table, set, payload)
	return err
}
