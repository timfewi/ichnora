package ingest

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"syscall"
)

// Cursor is the durable position in one native transcript file.
type Cursor struct {
	Path      string
	Device    int64
	Inode     int64
	Offset    int64
	LineNo    int64
	Malformed int64
	Oversized int64
	Rotations int64
	State     State
	// Skipping is set while discarding an oversized unterminated line.
	Skipping bool
}

type persisted struct {
	State
	Skipping bool `json:"skipping,omitempty"`
}

// StateJSON serializes adapter state for the cursor row.
func (c *Cursor) StateJSON() string {
	b, _ := json.Marshal(persisted{c.State, c.Skipping})
	return string(b)
}

// LoadState restores adapter state from the cursor row.
func (c *Cursor) LoadState(s string) {
	var p persisted
	if json.Unmarshal([]byte(s), &p) == nil {
		c.State, c.Skipping = p.State, p.Skipping
	}
}

// Line is one complete native line with its position.
type Line struct {
	No     int64
	Offset int64
	Data   []byte
}

// Increment reads complete lines after the cursor, at most maxBytes, and
// returns the advanced cursor. A trailing line without a newline is a partial
// write and stays unread. A replaced or truncated file restarts from zero;
// event identities make the re-read idempotent.
func Increment(cur Cursor, maxBytes int64, maxLine int) ([]Line, Cursor, error) {
	f, err := os.OpenFile(cur.Path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, cur, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, cur, err
	}
	if !st.Mode().IsRegular() {
		return nil, cur, errors.New("transcript is not a regular file")
	}
	next := cur
	if sys, ok := st.Sys().(*syscall.Stat_t); ok {
		dev, ino := int64(sys.Dev), int64(sys.Ino)
		if (cur.Inode != 0 && (dev != cur.Device || ino != cur.Inode)) || st.Size() < cur.Offset {
			next.Rotations++
			next.Offset, next.LineNo, next.Skipping = 0, 0, false
		}
		next.Device, next.Inode = dev, ino
	}
	if _, err := f.Seek(next.Offset, io.SeekStart); err != nil {
		return nil, cur, err
	}
	buf := make([]byte, maxBytes)
	n, err := io.ReadFull(f, buf)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return nil, cur, err
	}
	buf = buf[:n]
	var lines []Line
	pos := 0
	for pos < len(buf) {
		nl := bytes.IndexByte(buf[pos:], '\n')
		if nl < 0 {
			if len(buf)-pos > maxLine {
				// An oversized line still being written: discard it up to its newline.
				next.Oversized++
				next.Skipping = true
				pos = len(buf)
			}
			break
		}
		data := buf[pos : pos+nl]
		start := next.Offset + int64(pos)
		pos += nl + 1
		if next.Skipping {
			next.Skipping = false
			continue
		}
		next.LineNo++
		if len(data) > maxLine {
			next.Oversized++
			continue
		}
		if len(bytes.TrimSpace(data)) == 0 {
			continue
		}
		lines = append(lines, Line{No: next.LineNo, Offset: start, Data: data})
	}
	next.Offset += int64(pos)
	return lines, next, nil
}
