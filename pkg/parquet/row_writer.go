package parquet

import (
	"io"

	"github.com/parquet-go/parquet-go"
)

type RowWriterFlusher interface {
	parquet.RowWriter
	Flush() error
}

// CopyAsRowGroups copies row groups to dst from src and flush a rowgroup per rowGroupNumCount read.
// It returns the total number of rows copied and the number of row groups written.
// Flush is called on dst to finalize each row group.
func CopyAsRowGroups(dst RowWriterFlusher, src parquet.RowReader, rowGroupNumCount int) (total uint64, rowGroupCount uint64, err error) {
	if rowGroupNumCount <= 0 {
		panic("rowGroupNumCount must be positive")
	}
	bufferSize := defaultRowBufferSize
	if rowGroupNumCount < bufferSize {
		bufferSize = rowGroupNumCount
	}
	var buffer = make([]parquet.Row, bufferSize)
	if rrWithSchema, ok := src.(parquet.RowReaderWithSchema); ok {
		numCols := len(rrWithSchema.Schema().Columns())
		for i := range buffer {
			buffer[i] = make([]parquet.Value, 0, numCols)
		}
	} else {
		// If we didn't initialize the buffer elements to nil in the non-schema case,
		// we might end up with uninitialized or garbage values in the buffer.
		for i := range buffer {
			buffer[i] = nil
		}
	}

	var currentGroupCount int
	for {
		n, readErr := src.ReadRows(buffer[:bufferSize])
		if readErr != nil && readErr != io.EOF {
			return 0, 0, readErr
		}
		if n == 0 {
			break
		}
		buffer := buffer[:n]
		if currentGroupCount+n >= rowGroupNumCount {
			batchSize := rowGroupNumCount - currentGroupCount
			written, err := dst.WriteRows(buffer[:batchSize])
			if err != nil {
				return 0, 0, err
			}
			buffer = buffer[batchSize:]
			total += uint64(written)
			if err := dst.Flush(); err != nil {
				return 0, 0, err
			}
			rowGroupCount++
			currentGroupCount = 0
		}
		if len(buffer) == 0 {
			if readErr == io.EOF {
				break
			}
			continue
		}
		written, err := dst.WriteRows(buffer)
		if err != nil {
			return 0, 0, err
		}
		total += uint64(written)
		currentGroupCount += written
		if readErr == io.EOF {
			break
		}
	}
	if currentGroupCount > 0 {
		if err := dst.Flush(); err != nil {
			return 0, 0, err
		}
		rowGroupCount++
	}
	return
}
