package main

import (
	"bytes"
	"encoding/hex"
	"testing"
)

// A pack written by writePack must parse back to the same objects, with
// the trailer verified.
func TestPackRoundTrip(t *testing.T) {
	blob := []byte("hello world\n")
	blobID := hashObject(objBlob, blob)
	blobOID, err := hex.DecodeString(blobID)
	if err != nil {
		t.Fatal(err)
	}
	var tree bytes.Buffer
	tree.WriteString("100644 hello\x00")
	tree.Write(blobOID)
	treeID := hashObject(objTree, tree.Bytes())
	commit := []byte("tree " + treeID + "\nauthor a <a@a.invalid> 1700000000 +0000\ncommitter a <a@a.invalid> 1700000000 +0000\n\nmsg\n")
	commitID := hashObject(objCommit, commit)
	big := bytes.Repeat([]byte("abcdefgh"), 10000)
	bigID := hashObject(objBlob, big)
	objs := map[string]*Object{
		blobID:   {Type: objBlob, Data: blob},
		treeID:   {Type: objTree, Data: tree.Bytes()},
		commitID: {Type: objCommit, Data: commit},
		bigID:    {Type: objBlob, Data: big},
	}
	pack := writePack(objs)
	got, err := parsePack(pack)
	if err != nil {
		t.Fatalf("parsePack: %v", err)
	}
	if len(got) != len(objs) {
		t.Fatalf("got %d objects, want %d", len(got), len(objs))
	}
	for id, want := range objs {
		g, ok := got[id]
		if !ok {
			t.Fatalf("missing object %s", id)
		}
		if g.Type != want.Type || !bytes.Equal(g.Data, want.Data) {
			t.Fatalf("object %s mismatch", id)
		}
	}
}

// pkt-lines must round-trip, including flush and delim.
func TestPktLineRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	buf.Write(pktLineStr("command=fetch\n"))
	buf.Write(pktDelim())
	buf.Write(pktLineStr("want abc\n"))
	buf.Write(pktFlush())
	lines, err := decodePktLines(buf.Bytes())
	if err != nil {
		t.Fatalf("decodePktLines: %v", err)
	}
	if len(lines) != 2 || lines[0] != "command=fetch" || lines[1] != "want abc" {
		t.Fatalf("lines = %q", lines)
	}
}
