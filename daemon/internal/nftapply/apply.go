// Package nftapply applies rendered nftables rulesets and dumps/restores
// DNS-learned set elements across re-applies. All nft interaction goes
// through the nft binary; an interface keeps it mockable.
package nftapply

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os/exec"
	"strconv"
	"strings"
)

// Element is one nft set element with its remaining timeout.
type Element struct {
	IP         string
	TimeoutSec uint32 // configured timeout of the element (seconds)
	RemainSec  uint32 // remaining seconds (0 = static element)
}

// Backend executes nft operations.
type Backend interface {
	// Run executes nft with args, returning combined output.
	Run(ctx context.Context, args []string, stdin string) (string, error)
}

// ExecBackend runs the real nft binary.
type ExecBackend struct{}

// Run implements Backend.
func (ExecBackend) Run(ctx context.Context, args []string, stdin string) (string, error) {
	cmd := exec.CommandContext(ctx, "nft", args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	if err != nil {
		err = fmt.Errorf("nft %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(out.String()))
	}
	return out.String(), err
}

// Client applies and inspects the fencebreaker table.
type Client struct {
	Table   string
	Backend Backend
}

// New creates a client for the inet fencebreaker table.
func New(b Backend) *Client { return &Client{Table: "fencebreaker", Backend: b} }

// DeleteTable removes the whole table; missing table is not an error.
func (c *Client) DeleteTable(ctx context.Context) error {
	_, err := c.Backend.Run(ctx, []string{"delete", "table", "inet", c.Table}, "")
	if err != nil && strings.Contains(err.Error(), "No such file or directory") {
		return nil
	}
	return err
}

// Apply installs a complete ruleset text atomically.
func (c *Client) Apply(ctx context.Context, ruleset string) error {
	_, err := c.Backend.Run(ctx, []string{"-f", "-"}, ruleset)
	return err
}

// DumpSets returns the contents of the given sets (name → elements).
// Missing table or set yields an empty map.
type nftJSON struct {
	Nftables []struct {
		Set *struct {
			Name string `json:"name"`
			Elem []struct {
				Elem *struct {
					Val    interface{}            `json:"val"`
					Timeout *float64              `json:"timeout"`
					Expires *float64              `json:"expires"`
				} `json:"elem"`
			} `json:"elem"`
		} `json:"set"`
	} `json:"nftables"`
}

// DumpSets dumps the elements of every named set from the live table.
func (c *Client) DumpSets(ctx context.Context, setNames []string) (map[string][]Element, error) {
	out := map[string][]Element{}
	for _, name := range setNames {
		elems, err := c.dumpSet(ctx, name)
		if err != nil {
			return nil, err
		}
		out[name] = elems
	}
	return out, nil
}

func (c *Client) dumpSet(ctx context.Context, name string) ([]Element, error) {
	raw, err := c.Backend.Run(ctx, []string{"-j", "list", "set", "inet", c.Table, name}, "")
	if err != nil {
		// missing table/set — nothing learned yet
		return nil, nil
	}
	var doc nftJSON
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		return nil, fmt.Errorf("parse nft json for set %s: %w", name, err)
	}
	var out []Element
	for _, item := range doc.Nftables {
		if item.Set == nil || item.Set.Name != name || len(item.Set.Elem) == 0 {
			continue
		}
		for _, e := range item.Set.Elem {
			if e.Elem == nil {
				continue
			}
			ip := elemIP(e.Elem.Val)
			if ip == "" {
				continue
			}
			el := Element{IP: ip}
			if e.Elem.Timeout != nil {
				el.TimeoutSec = uint32(*e.Elem.Timeout)
			}
			if e.Elem.Expires != nil {
				el.RemainSec = uint32(*e.Elem.Expires)
			}
			out = append(out, el)
		}
	}
	return out, nil
}

func elemIP(v interface{}) string {
	switch val := v.(type) {
	case string:
		if ip := net.ParseIP(val); ip != nil {
			if v4 := ip.To4(); v4 != nil {
				return v4.String()
			}
		}
	case []interface{}:
		if len(val) == 4 {
			var parts []string
			for _, p := range val {
				s, ok := p.(float64)
				if !ok {
					return ""
				}
				parts = append(parts, strconv.Itoa(int(s)))
			}
			ip := net.ParseIP(strings.Join(parts, "."))
			if ip != nil {
				return ip.To4().String()
			}
		}
	}
	return ""
}

// RestoreTimeout picks the timeout for a dumped element on restore: keep the
// remaining lifetime, floored at one second.
func RestoreTimeout(e Element) uint32 {
	if e.RemainSec > 0 {
		return e.RemainSec
	}
	if e.TimeoutSec > 0 {
		return e.TimeoutSec
	}
	return 300
}
