package rcsandroid

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Rect is a screen rectangle in pixels.
type Rect struct{ L, T, R, B int }

func (r Rect) Center() (int, int) { return (r.L + r.R) / 2, (r.T + r.B) / 2 }

// Node is one element of a uiautomator dump.
type Node struct {
	Class      string
	Package    string
	ResourceID string
	Text       string
	Desc       string
	Bounds     Rect
	Clickable  bool
	Enabled    bool
}

// ID is the resource id without its package prefix ("compose_message_text").
func (n Node) ID() string {
	if i := strings.LastIndex(n.ResourceID, "id/"); i >= 0 {
		return n.ResourceID[i+3:]
	}
	return n.ResourceID
}

// Screen is every node of one dump, in document order.
type Screen struct{ Nodes []Node }

type xmlNode struct {
	Class      string    `xml:"class,attr"`
	Package    string    `xml:"package,attr"`
	ResourceID string    `xml:"resource-id,attr"`
	Text       string    `xml:"text,attr"`
	Desc       string    `xml:"content-desc,attr"`
	Bounds     string    `xml:"bounds,attr"`
	Clickable  string    `xml:"clickable,attr"`
	Enabled    string    `xml:"enabled,attr"`
	Children   []xmlNode `xml:"node"`
}

type xmlHierarchy struct {
	Nodes []xmlNode `xml:"node"`
}

var boundsRe = regexp.MustCompile(`\[(-?\d+),(-?\d+)\]\[(-?\d+),(-?\d+)\]`)

// ErrNoHierarchy means the output held no uiautomator XML (the screen was
// off, the device refused, or the command printed an error instead).
var ErrNoHierarchy = errors.New("no uiautomator hierarchy in the output")

// ParseScreen reads the XML that `uiautomator dump` prints. The command
// follows the XML with a status line, and some devices put noise before it,
// so only the <hierarchy> element is parsed.
func ParseScreen(raw []byte) (*Screen, error) {
	start := bytes.Index(raw, []byte("<hierarchy"))
	end := bytes.LastIndex(raw, []byte("</hierarchy>"))
	if start < 0 || end < start {
		return nil, ErrNoHierarchy
	}
	var h xmlHierarchy
	if err := xml.Unmarshal(raw[start:end+len("</hierarchy>")], &h); err != nil {
		return nil, fmt.Errorf("reading uiautomator xml: %w", err)
	}
	s := &Screen{}
	var walk func(ns []xmlNode)
	walk = func(ns []xmlNode) {
		for _, n := range ns {
			s.Nodes = append(s.Nodes, Node{
				Class: n.Class, Package: n.Package, ResourceID: n.ResourceID, Text: n.Text, Desc: n.Desc,
				Bounds: parseBounds(n.Bounds), Clickable: n.Clickable == "true", Enabled: n.Enabled != "false",
			})
			walk(n.Children)
		}
	}
	walk(h.Nodes)
	return s, nil
}

func parseBounds(s string) Rect {
	m := boundsRe.FindStringSubmatch(s)
	if m == nil {
		return Rect{}
	}
	v := func(i int) int { n, _ := strconv.Atoi(m[i]); return n }
	return Rect{v(1), v(2), v(3), v(4)}
}

// Find returns the first node for which match is true.
func (s *Screen) Find(match func(Node) bool) (Node, bool) {
	for _, n := range s.Nodes {
		if match(n) {
			return n, true
		}
	}
	return Node{}, false
}

// FindAll returns every node for which match is true.
func (s *Screen) FindAll(match func(Node) bool) []Node {
	var out []Node
	for _, n := range s.Nodes {
		if match(n) {
			out = append(out, n)
		}
	}
	return out
}
