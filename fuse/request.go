package fuse

import (
	"bytes"
	"fmt"
	"log"
	"strings"
	"time"
	"unsafe"
	"github.com/jbooth/maggiefs/splice"
)

var sizeOfOutHeader = unsafe.Sizeof(OutHeader{})
var zeroOutBuf [outputHeaderSize]byte

type request struct {
	inputBuf []byte

	// These split up inputBuf.
	inHeader *InHeader      // generic header
	inData   unsafe.Pointer // per op data
	arg      []byte         // flat data.

	filenames []string // filename arguments

	// Unstructured data, a pointer to the relevant XxxxOut struct.
	outData  unsafe.Pointer
	status   Status
	flatData []byte

	// In case of read, keep read result buffer here so we can call
	// splice on it
	readNumBytesInChan int
	readBuffer *splice.Pair

	// Start timestamp for timing info.
	startTime time.Time

	// All information pertaining to opcode of this request.
	handler *operationHandler

	// Request storage. For large inputs and outputs, use data
	// obtained through bufferpool.
	bufferPoolInputBuf  []byte
	bufferPoolOutputBuf []byte

	// For small pieces of data, we use the following inlines
	// arrays:
	//
	// Output header and structured data.
	outBuf [outputHeaderSize]byte

	// Input, if small enough to fit here.
	smallInputBuf [128]byte

	context Context
}

func (r *request) clear() {
	r.inputBuf = nil
	r.inHeader = nil
	r.inData = nil
	r.arg = nil
	r.filenames = nil
	r.outData = nil
	r.status = OK
	r.flatData = nil
	r.readBuffer = nil
	r.startTime = time.Time{}
	r.handler = nil
}

func (r *request) InputDebug() string {
	val := " "
	if r.handler.DecodeIn != nil {
		val = fmt.Sprintf(" data: %v ", Print(r.handler.DecodeIn(r.inData)))
	}

	names := ""
	if r.filenames != nil {
		names = fmt.Sprintf("names: %v", r.filenames)
	}

	if len(r.arg) > 0 {
		names += fmt.Sprintf(" %d bytes", len(r.arg))
	}

	return fmt.Sprintf("Dispatch: %s, NodeId: %v.%v%v",
		operationName(r.inHeader.Opcode), r.inHeader.NodeId, val, names)
}

func (r *request) OutputDebug() string {
	var dataStr string
	if r.handler.DecodeOut != nil && r.outData != nil {
		dataStr = Print(r.handler.DecodeOut(r.outData))
	}

	max := 1024
	if len(dataStr) > max {
		dataStr = dataStr[:max] + fmt.Sprintf(" ...trimmed")
	}

	flatStr := ""
	if r.flatDataSize() > 0 {
		if r.handler.FileNameOut {
			s := strings.TrimRight(string(r.flatData), "\x00")
			flatStr = fmt.Sprintf(" %q", s)
		} else {
			if r.readBuffer != nil {
				flatStr = fmt.Sprintf(" (has pipe buffer with %d bytes in chan)",r.readNumBytesInChan)
			}
		}
	}

	return fmt.Sprintf("Serialize: %s code: %v value: %v%v",
		operationName(r.inHeader.Opcode), r.status, dataStr, flatStr)
}

// setInput returns true if it takes ownership of the argument, false if not.
func (r *request) setInput(input []byte) bool {
	if len(input) < len(r.smallInputBuf) {
		copy(r.smallInputBuf[:], input)
		r.inputBuf = r.smallInputBuf[:len(input)]
		return false
	}
	r.inputBuf = input
	r.bufferPoolInputBuf = input

	return true
}

func (r *request) parse() {
	inHSize := int(unsafe.Sizeof(InHeader{}))
	if len(r.inputBuf) < inHSize {
		log.Printf("Short read for input header: %v", r.inputBuf)
		return
	}

	r.inHeader = (*InHeader)(unsafe.Pointer(&r.inputBuf[0]))
	r.arg = r.inputBuf[:]

	r.handler = getHandler(r.inHeader.Opcode)
	if r.handler == nil {
		log.Printf("Unknown opcode %d", r.inHeader.Opcode)
		r.status = ENOSYS
		return
	}

	if len(r.arg) < int(r.handler.InputSize) {
		log.Printf("Short read for %v: %v", operationName(r.inHeader.Opcode), r.arg)
		r.status = EIO
		return
	}

	if r.handler.InputSize > 0 {
		r.inData = unsafe.Pointer(&r.arg[0])
		r.arg = r.arg[r.handler.InputSize:]
	} else {
		r.arg = r.arg[inHSize:]
	}

	count := r.handler.FileNames
	if count > 0 {
		if count == 1 {
			r.filenames = []string{string(r.arg[:len(r.arg)-1])}
		} else {
			names := bytes.SplitN(r.arg[:len(r.arg)-1], []byte{0}, count)
			r.filenames = make([]string, len(names))
			for i, n := range names {
				r.filenames[i] = string(n)
			}
			if len(names) != count {
				log.Println("filename argument mismatch", names, count)
				r.status = EIO
			}
		}
	}

	copy(r.outBuf[:r.handler.OutputSize], zeroOutBuf[:r.handler.OutputSize])
	r.outData = unsafe.Pointer(&r.outBuf[sizeOfOutHeader])
}

func (r *request) serializeHeader(dataSize int) (header []byte) {
	dataLength := r.handler.OutputSize
	if r.outData == nil || r.status > OK {
		dataLength = 0
	}

	sizeOfOutHeader := unsafe.Sizeof(OutHeader{})
	header = r.outBuf[:sizeOfOutHeader+dataLength]
	o := (*OutHeader)(unsafe.Pointer(&header[0]))
	o.Unique = r.inHeader.Unique
	o.Status = int32(-r.status)
	o.Length = uint32(
		int(sizeOfOutHeader) + int(dataLength) + dataSize)

	var asSlice []byte
	toSlice(&asSlice, r.outData, dataLength)
	copy(header[sizeOfOutHeader:], asSlice)
	return header
}

func (r *request) flatDataSize() int {
	if r.readBuffer != nil {
		return int(r.readNumBytesInChan)
	}
	return len(r.flatData)
}