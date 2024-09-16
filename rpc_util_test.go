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
	"time"

	"github.com/stretchr/testify/assert"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/encoding"
	protoenc "google.golang.org/grpc/encoding/proto"
	"google.golang.org/grpc/internal/testutils"
	"google.golang.org/grpc/internal/transport"
	"google.golang.org/grpc/mem"
	"google.golang.org/grpc/stats"
	"google.golang.org/grpc/status"
	perfpb "google.golang.org/grpc/test/codec_perf"
	"google.golang.org/protobuf/proto"
)

type fullReader struct {
	data []byte
}

func (f *fullReader) ReadHeader(header []byte) error {
	buf, err := f.Read(len(header))
	defer buf.Free()
	if err != nil {
		return err
	}

	buf.CopyTo(header)
	return nil
}

func (f *fullReader) Read(n int) (mem.BufferSlice, error) {
	if len(f.data) == 0 {
		return nil, io.EOF
	}

	if len(f.data) < n {
		data := f.data
		f.data = nil
		return mem.BufferSlice{mem.NewBuffer(&data, nil)}, io.ErrUnexpectedEOF
	}

	buf := f.data[:n]
	f.data = f.data[n:]

	return mem.BufferSlice{mem.NewBuffer(&buf, nil)}, nil
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
		buf := &fullReader{test.p}
		parser := &parser{r: buf, bufferPool: mem.DefaultBufferPool()}
		pt, b, err := parser.recvMsg(math.MaxInt32)
		if err != test.err || !bytes.Equal(b.Materialize(), test.b) || pt != test.pt {
			t.Fatalf("parser{%v}.recvMsg(_) = %v, %v, %v\nwant %v, %v, %v", test.p, pt, b, err, test.pt, test.b, test.err)
		}
	}
}

func (s) TestMultipleParsing(t *testing.T) {
	// Set a byte stream consists of 3 messages with their headers.
	p := []byte{0, 0, 0, 0, 1, 'a', 0, 0, 0, 0, 2, 'b', 'c', 0, 0, 0, 0, 1, 'd'}
	b := &fullReader{p}
	parser := &parser{r: b, bufferPool: mem.DefaultBufferPool()}

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
		if err != nil || pt != want.pt || !reflect.DeepEqual(data.Materialize(), want.data) {
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
		data, err := encode(getCodec(protoenc.Name), test.msg)
		if err != test.err || !bytes.Equal(data.Materialize(), test.data) {
			t.Errorf("encode(_, %v) = %v, %v; want %v, %v", test.msg, data, err, test.data, test.err)
			continue
		}
		if hdr, _ := msgHeader(data, nil, compressionNone); !bytes.Equal(hdr, test.hdr) {
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
	cdc := getCodec(protoenc.Name)
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
				t.Errorf("unexpected error state: want err=%v, got err=%v", tt.wantErr, err)
			} else if err != nil && err.Error() != tt.wantErr.Error() {
				t.Errorf("unexpected error message: want err=%v, got err=%v", tt.wantErr, err)
			}

		})
	}
}

// // GzipCompressor implements encoding.Compressor for gzip compression.
// type GzipCompressor struct{}

// func (c *GzipCompressor) Compress(w io.Writer) (io.WriteCloser, error) {
// 	return gzip.NewWriter(w), nil
// }

// func (c *GzipCompressor) Decompress(r io.Reader) (io.Reader, error) {
// 	return gzip.NewReader(r)
// }

// // TestDecompressReader tests the specific line: dcReader, err := compressor.Decompress(d.Reader()).
// func TestDecompressReader(t *testing.T) {
// 	compressor := &GzipCompressor{}
// 	message := "hello world"

// 	// Step 1: Compress the message to simulate valid compressed data
// 	var compressedDataBuffer bytes.Buffer
// 	writer := gzip.NewWriter(&compressedDataBuffer)
// 	_, err := writer.Write([]byte(message))
// 	if err != nil {
// 		t.Fatalf("failed to compress data: %v", err)
// 	}
// 	writer.Close()

// 	// Step 2: Convert []byte to *[]byte by taking the address of compressedDataBuffer.Bytes()
// 	compressedBytes := compressedDataBuffer.Bytes()
// 	compressedData := mem.NewBuffer(&compressedBytes, nil)
// 	bufferSlice := mem.BufferSlice{compressedData}

// 	// Step 3: Call compressor.Decompress with bufferSlice.Reader()
// 	dcReader, err := compressor.Decompress(bufferSlice.Reader())
// 	if err != nil {
// 		t.Fatalf("compressor.Decompress failed: %v", err)
// 	}

// 	// Step 4: Validate that dcReader is correctly reading the decompressed data
// 	var decompressedData bytes.Buffer
// 	_, err = io.Copy(&decompressedData, dcReader)
// 	if err != nil {
// 		t.Fatalf("failed to read from dcReader: %v", err)
// 	}

// 	// Step 5: Compare the decompressed data to the original message
// 	if decompressedData.String() != message {
// 		t.Errorf("expected decompressed data to be %q, but got %q", message, decompressedData.String())
// 	}
// }

// // TestDecompressReaderError tests the error case where the data is not valid compressed data.
// func TestDecompressReaderError(t *testing.T) {
// 	compressor := &GzipCompressor{}
// 	invalidData := []byte("invalid compressed data")

// 	// Step 1: Create a mem.BufferSlice from invalid data
// 	invalidBuffer := mem.NewBuffer(&invalidData, nil)
// 	invalidBufferSlice := mem.BufferSlice{invalidBuffer}

// 	// Step 2: Attempt to decompress invalid data
// 	_, err := compressor.Decompress(invalidBufferSlice.Reader())

// 	// Step 3: Validate that an error occurred
// 	if err == nil {
// 		t.Fatal("expected an error due to invalid compressed data, but got none")
// 	}
// }

func TestOutPayload(t *testing.T) {
	// Test data
	client := true
	msg := "test message"
	dataLength := 100
	payloadLength := 150
	headerLen := 5 // Assuming this is the constant value used in the function
	timestamp := time.Now()

	// Expected output
	expected := &stats.OutPayload{
		Client:           client,
		Payload:          msg,
		Length:           dataLength,
		WireLength:       payloadLength + headerLen,
		CompressedLength: payloadLength,
		SentTime:         timestamp,
	}

	// Call the function
	result := outPayload(client, msg, dataLength, payloadLength, timestamp)

	// Validate the result using assertions
	assert.Equal(t, expected.Client, result.Client, "Client mismatch")
	assert.Equal(t, expected.Payload, result.Payload, "Payload mismatch")
	assert.Equal(t, expected.Length, result.Length, "Length mismatch")
	assert.Equal(t, expected.WireLength, result.WireLength, "WireLength mismatch")
	assert.Equal(t, expected.CompressedLength, result.CompressedLength, "CompressedLength mismatch")
	assert.Equal(t, expected.SentTime, result.SentTime, "SentTime mismatch")
}

func TestCheckRecvPayload(t *testing.T) {
	// Define constants used in tests
	compressionNone := payloadFormat(0)
	compressionMade := payloadFormat(1)

	tests := []struct {
		name           string
		pf             payloadFormat
		recvCompress   string
		haveCompressor bool
		isServer       bool
		expectedCode   codes.Code
		expectedMsg    string
	}{
		{
			name:           "No compression (compressionNone)",
			pf:             compressionNone,
			recvCompress:   "",
			haveCompressor: false,
			isServer:       false,
			expectedCode:   codes.OK,
		},
		{
			name:           "Compressed flag with empty encoding (compressionMade)",
			pf:             compressionMade,
			recvCompress:   "",
			haveCompressor: false,
			isServer:       false,
			expectedCode:   codes.Internal,
			expectedMsg:    "grpc: compressed flag set with identity or empty encoding",
		},
		{
			name:           "Compressed flag with identity encoding (compressionMade)",
			pf:             compressionMade,
			recvCompress:   encoding.Identity,
			haveCompressor: false,
			isServer:       false,
			expectedCode:   codes.Internal,
			expectedMsg:    "grpc: compressed flag set with identity or empty encoding",
		},
		{
			name:           "Compressor not installed (client)",
			pf:             compressionMade,
			recvCompress:   "gzip",
			haveCompressor: false,
			isServer:       false,
			expectedCode:   codes.Internal,
			expectedMsg:    "grpc: Decompressor is not installed for grpc-encoding \"gzip\"",
		},
		{
			name:           "Compressor not installed (server)",
			pf:             compressionMade,
			recvCompress:   "gzip",
			haveCompressor: false,
			isServer:       true,
			expectedCode:   codes.Unimplemented,
			expectedMsg:    "grpc: Decompressor is not installed for grpc-encoding \"gzip\"",
		},
		{
			name:           "Unexpected payload format",
			pf:             payloadFormat(2),
			recvCompress:   "",
			haveCompressor: false,
			isServer:       false,
			expectedCode:   codes.Internal,
			expectedMsg:    "grpc: received unexpected payload format 2",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Call the function
			result := checkRecvPayload(tt.pf, tt.recvCompress, tt.haveCompressor, tt.isServer)

			// Check if result is nil for OK status
			if tt.expectedCode == codes.OK {
				assert.Nil(t, result)
			} else {
				// Validate non-nil status and its code/message
				assert.NotNil(t, result)
				assert.Equal(t, tt.expectedCode, result.Code(), "Unexpected status code")
				assert.Contains(t, result.Message(), tt.expectedMsg, "Unexpected status message")
			}
		})
	}
}

// Dummy decompressor function (replace with the actual decompressor from your code)
func Decompress(r io.Reader) (io.Reader, error) {
	return gzip.NewReader(r)
}

func TestDecompressWithMemBufferSlice(t *testing.T) {
	// Step 1: Create a buffer with compressed data
	var compressedData bytes.Buffer
	w := gzip.NewWriter(&compressedData)
	_, err := w.Write([]byte("test data"))
	assert.NoError(t, err)
	w.Close()
	// var compressedDataBuffer bytes.Buffer
	// Step 2: Create mem.BufferSlice using the compressed data
	//  data := compressedData.Bytes()
	// compressedBytes := compressedData.Bytes()
	var bufferSlice mem.BufferSlice
	// bufferSlice = mem.BufferSlice{
	// 	Buffers:[]byte{compressedBytes}, // Create BufferSlice from compressed data
	// }

	// Step 3: Call the Decompress method
	dcReader, err := Decompress(bufferSlice.Reader())
	assert.NoError(t, err)

	// Step 4: Read and verify the decompressed data
	var decompressedData bytes.Buffer
	_, err = io.Copy(&decompressedData, dcReader)
	assert.NoError(t, err)

	// Step 5: Compare the decompressed output with the expected result
	expected := "test data"
	assert.Equal(t, expected, decompressedData.String())
}
