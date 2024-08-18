/*
 *
 * Copyright 2014 gRPC authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 */

package grpc

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"math"
	"reflect"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/encoding"
	protoenc "google.golang.org/grpc/encoding/proto"
	"google.golang.org/grpc/internal/testutils"
	"google.golang.org/grpc/internal/transport"
	"google.golang.org/grpc/status"
	perfpb "google.golang.org/grpc/test/codec_perf"
	"google.golang.org/protobuf/proto"
)

type fullReader struct {
	reader io.Reader
}

func (f fullReader) Read(p []byte) (int, error) {
	return io.ReadFull(f.reader, p)
}

var _ CallOption = EmptyCallOption{} // ensure EmptyCallOption implements the interface

func (s) TestSimpleParsing(t *testing.T) {
	bigMsg := bytes.Repeat([]byte{'x'}, 1<<24)
	for _, test := range []struct {
		// input
		p []byte
		// outputs
		err error
		b   []byte
		pt  payloadFormat
	}{
		{nil, io.EOF, nil, compressionNone},
		{[]byte{0, 0, 0, 0, 0}, nil, nil, compressionNone},
		{[]byte{0, 0, 0, 0, 1, 'a'}, nil, []byte{'a'}, compressionNone},
		{[]byte{1, 0}, io.ErrUnexpectedEOF, nil, compressionNone},
		{[]byte{0, 0, 0, 0, 10, 'a'}, io.ErrUnexpectedEOF, nil, compressionNone},
		// Check that messages with length >= 2^24 are parsed.
		{append([]byte{0, 1, 0, 0, 0}, bigMsg...), nil, bigMsg, compressionNone},
	} {
		buf := fullReader{bytes.NewReader(test.p)}
		parser := &parser{r: buf, recvBufferPool: nopBufferPool{}}
		pt, b, err := parser.recvMsg(math.MaxInt32)
		if err != test.err || !bytes.Equal(b, test.b) || pt != test.pt {
			t.Fatalf("parser{%v}.recvMsg(_) = %v, %v, %v\nwant %v, %v, %v", test.p, pt, b, err, test.pt, test.b, test.err)
		}
	}
}

func (s) TestMultipleParsing(t *testing.T) {
	// Set a byte stream consists of 3 messages with their headers.
	p := []byte{0, 0, 0, 0, 1, 'a', 0, 0, 0, 0, 2, 'b', 'c', 0, 0, 0, 0, 1, 'd'}
	b := fullReader{bytes.NewReader(p)}
	parser := &parser{r: b, recvBufferPool: nopBufferPool{}}

	wantRecvs := []struct {
		pt   payloadFormat
		data []byte
	}{
		{compressionNone, []byte("a")},
		{compressionNone, []byte("bc")},
		{compressionNone, []byte("d")},
	}
	for i, want := range wantRecvs {
		pt, data, err := parser.recvMsg(math.MaxInt32)
		if err != nil || pt != want.pt || !reflect.DeepEqual(data, want.data) {
			t.Fatalf("after %d calls, parser{%v}.recvMsg(_) = %v, %v, %v\nwant %v, %v, <nil>",
				i, p, pt, data, err, want.pt, want.data)
		}
	}

	pt, data, err := parser.recvMsg(math.MaxInt32)
	if err != io.EOF {
		t.Fatalf("after %d recvMsgs calls, parser{%v}.recvMsg(_) = %v, %v, %v\nwant _, _, %v",
			len(wantRecvs), p, pt, data, err, io.EOF)
	}
}

func (s) TestEncode(t *testing.T) {
	for _, test := range []struct {
		// input
		msg proto.Message
		// outputs
		hdr  []byte
		data []byte
		err  error
	}{
		{nil, []byte{0, 0, 0, 0, 0}, []byte{}, nil},
	} {
		data, err := encode(encoding.GetCodec(protoenc.Name), test.msg)
		if err != test.err || !bytes.Equal(data, test.data) {
			t.Errorf("encode(_, %v) = %v, %v; want %v, %v", test.msg, data, err, test.data, test.err)
			continue
		}
		if hdr, _ := msgHeader(data, nil); !bytes.Equal(hdr, test.hdr) {
			t.Errorf("msgHeader(%v, false) = %v; want %v", data, hdr, test.hdr)
		}
	}
}

func (s) TestCompress(t *testing.T) {
	bestCompressor, err := NewGZIPCompressorWithLevel(gzip.BestCompression)
	if err != nil {
		t.Fatalf("Could not initialize gzip compressor with best compression.")
	}
	bestSpeedCompressor, err := NewGZIPCompressorWithLevel(gzip.BestSpeed)
	if err != nil {
		t.Fatalf("Could not initialize gzip compressor with best speed compression.")
	}

	defaultCompressor, err := NewGZIPCompressorWithLevel(gzip.BestSpeed)
	if err != nil {
		t.Fatalf("Could not initialize gzip compressor with default compression.")
	}

	level5, err := NewGZIPCompressorWithLevel(5)
	if err != nil {
		t.Fatalf("Could not initialize gzip compressor with level 5 compression.")
	}

	for _, test := range []struct {
		// input
		data []byte
		cp   Compressor
		dc   Decompressor
		// outputs
		err error
	}{
		{make([]byte, 1024), NewGZIPCompressor(), NewGZIPDecompressor(), nil},
		{make([]byte, 1024), bestCompressor, NewGZIPDecompressor(), nil},
		{make([]byte, 1024), bestSpeedCompressor, NewGZIPDecompressor(), nil},
		{make([]byte, 1024), defaultCompressor, NewGZIPDecompressor(), nil},
		{make([]byte, 1024), level5, NewGZIPDecompressor(), nil},
	} {
		b := new(bytes.Buffer)
		if err := test.cp.Do(b, test.data); err != test.err {
			t.Fatalf("Compressor.Do(_, %v) = %v, want %v", test.data, err, test.err)
		}
		if b.Len() >= len(test.data) {
			t.Fatalf("The compressor fails to compress data.")
		}
		if p, err := test.dc.Do(b); err != nil || !bytes.Equal(test.data, p) {
			t.Fatalf("Decompressor.Do(%v) = %v, %v, want %v, <nil>", b, p, err, test.data)
		}
	}
}

func (s) TestToRPCErr(t *testing.T) {
	for _, test := range []struct {
		// input
		errIn error
		// outputs
		errOut error
	}{
		{transport.ErrConnClosing, status.Error(codes.Unavailable, transport.ErrConnClosing.Desc)},
		{io.ErrUnexpectedEOF, status.Error(codes.Internal, io.ErrUnexpectedEOF.Error())},
	} {
		err := toRPCErr(test.errIn)
		if _, ok := status.FromError(err); !ok {
			t.Errorf("toRPCErr{%v} returned type %T, want %T", test.errIn, err, status.Error)
		}
		if !testutils.StatusErrEqual(err, test.errOut) {
			t.Errorf("toRPCErr{%v} = %v \nwant %v", test.errIn, err, test.errOut)
		}
	}
}

// bmEncode benchmarks encoding a Protocol Buffer message containing mSize
// bytes.
func bmEncode(b *testing.B, mSize int) {
	cdc := encoding.GetCodec(protoenc.Name)
	msg := &perfpb.Buffer{Body: make([]byte, mSize)}
	encodeData, _ := encode(cdc, msg)
	encodedSz := int64(len(encodeData))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		encode(cdc, msg)
	}
	b.SetBytes(encodedSz)
}

func BenchmarkEncode1B(b *testing.B) {
	bmEncode(b, 1)
}

func BenchmarkEncode1KiB(b *testing.B) {
	bmEncode(b, 1024)
}

func BenchmarkEncode8KiB(b *testing.B) {
	bmEncode(b, 8*1024)
}

func BenchmarkEncode64KiB(b *testing.B) {
	bmEncode(b, 64*1024)
}

func BenchmarkEncode512KiB(b *testing.B) {
	bmEncode(b, 512*1024)
}

func BenchmarkEncode1MiB(b *testing.B) {
	bmEncode(b, 1024*1024)
}

// bmCompressor benchmarks a compressor of a Protocol Buffer message containing
// mSize bytes.
func bmCompressor(b *testing.B, mSize int, cp Compressor) {
	payload := make([]byte, mSize)
	cBuf := bytes.NewBuffer(make([]byte, mSize))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		cp.Do(cBuf, payload)
		cBuf.Reset()
	}
}

func BenchmarkGZIPCompressor1B(b *testing.B) {
	bmCompressor(b, 1, NewGZIPCompressor())
}

func BenchmarkGZIPCompressor1KiB(b *testing.B) {
	bmCompressor(b, 1024, NewGZIPCompressor())
}

func BenchmarkGZIPCompressor8KiB(b *testing.B) {
	bmCompressor(b, 8*1024, NewGZIPCompressor())
}

func BenchmarkGZIPCompressor64KiB(b *testing.B) {
	bmCompressor(b, 64*1024, NewGZIPCompressor())
}

func BenchmarkGZIPCompressor512KiB(b *testing.B) {
	bmCompressor(b, 512*1024, NewGZIPCompressor())
}

func BenchmarkGZIPCompressor1MiB(b *testing.B) {
	bmCompressor(b, 1024*1024, NewGZIPCompressor())
}
func TestCheckReceiveMessageOverflow(t *testing.T) {
	tests := []struct {
		name                  string
		readBytes             int64
		maxReceiveMessageSize int64
		dcReader              io.Reader
		wantErr               error
	}{
		{
			name:                  "No overflow",
			readBytes:             5,
			maxReceiveMessageSize: 10,
			dcReader:              bytes.NewReader([]byte{}),
			wantErr:               nil,
		},
		{
			name:                  "Overflow with additional data",
			readBytes:             10,
			maxReceiveMessageSize: 10,
			dcReader:              bytes.NewReader([]byte{1}),
			wantErr:               errors.New("overflow: message larger than max size receivable by client (10 bytes)"),
		},
		{
			name:                  "No overflow with EOF",
			maxReceiveMessageSize: 10,
			dcReader:              bytes.NewReader([]byte{}),
			wantErr:               nil,
		},
		{
			name:                  "Overflow condition with error handling",
			readBytes:             15,
			maxReceiveMessageSize: 15,
			dcReader:              bytes.NewReader([]byte{1, 2, 3}),
			wantErr:               errors.New("overflow: message larger than max size receivable by client (15 bytes)"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkReceiveMessageOverflow(tt.readBytes, tt.maxReceiveMessageSize, tt.dcReader)
			if (err != nil) != (tt.wantErr != nil) {
				t.Errorf("unexpected error state: got err=%v, want err=%v", err, tt.wantErr)
			} else if err != nil && err.Error() != tt.wantErr.Error() {
				t.Errorf("unexpected error message: got err=%v, want err=%v", err, tt.wantErr)
			}

		})
	}
}

type testCompressor struct {
	triggerDecompressError bool
}

func (c *testCompressor) Name() string {
	// Return a name for the compressor.
	return "testCompressor"
}

func (c *testCompressor) Compress(w io.Writer) (io.WriteCloser, error) {
	return nil, errors.New("Compress not implemented")
}

func (c *testCompressor) Decompress(r io.Reader) (io.Reader, error) {
	if c.triggerDecompressError {
		return nil, errors.New("decompression failed")
	}
	return r, nil
}

func (c *testCompressor) DecompressedSize(compressedBytes []byte) int {
	return len(compressedBytes) * 2 // Assume decompressed size is double for testing
}

// TestDecompress tests the decompress function.
func TestDecompress(t *testing.T) {
	tests := []struct {
		name                  string
		compressor            *testCompressor
		input                 []byte
		maxReceiveMessageSize int
		wantOutput            []byte
		wantSize              int
		wantErr               bool
	}{
		{
			name:                  "Successful decompression",
			compressor:            &testCompressor{},
			input:                 []byte{0x01, 0x02, 0x03, 0x04},
			maxReceiveMessageSize: 10,
			wantOutput:            []byte{0x01, 0x02, 0x03, 0x04},
			wantSize:              4,
			wantErr:               false,
		},
		{
			name:                  "Message size overflow",
			compressor:            &testCompressor{},
			input:                 []byte{0x01, 0x02, 0x03, 0x04},
			maxReceiveMessageSize: 2,
			wantOutput:            nil,
			wantSize:              8,
			wantErr:               true,
		},
		{
			name:                  "Error during decompression",
			compressor:            &testCompressor{triggerDecompressError: true},
			input:                 []byte{0x01, 0x02, 0x03, 0x04},
			maxReceiveMessageSize: 10,
			wantOutput:            nil,
			wantSize:              0,
			wantErr:               true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			output, size, err := decompress(tt.compressor, tt.input, tt.maxReceiveMessageSize)

			if (err != nil) != tt.wantErr {
				t.Errorf("decompress() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !bytes.Equal(output, tt.wantOutput) {
				t.Errorf("decompress() got = %v, want %v", output, tt.wantOutput)
			}
			if size != tt.wantSize {
				t.Errorf("decompress() size = %d, want %d", size, tt.wantSize)
			}
		})
	}
}
