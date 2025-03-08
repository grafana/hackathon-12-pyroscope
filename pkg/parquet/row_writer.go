// Package parquet provides utilities for reading/writing Parquet rows.
package parquet

import (
	"io"

	"github.com/parquet-go/parquet-go"
)

// RowWriterFlusher extends parquet.RowWriter with an explicit Flush method to
// start a new row group in the output.
type RowWriterFlusher interface {
	parquet.RowWriter
	Flush() error
}

// CopyAsRowGroups reads rows from src and writes them to dst, flushing (ending the
// current row group) every rowGroupNumCount rows. It returns the total number of rows
// copied and the number of row groups written. Flush is called on dst to finalize each row group.
func CopyAsRowGroups(dst RowWriterFlusher, src parquet.RowReader, rowGroupNumCount int) (total uint64, rowGroupCount uint64, err error) {
	if rowGroupNumCount <= 0 {
		panic("rowGroupNumCount must be positive")
	}

	// Determine buffer size: use a larger default batch size for efficiency, but
	// don’t exceed the rowGroupNumCount (to avoid overly large allocations for small groups).
	bufferSize := defaultRowBufferSize
	if rowGroupNumCount < bufferSize {
		bufferSize = rowGroupNumCount
	}

	// Preallocate a slice of parquet.Row to serve as our read buffer.
	// Each parquet.Row is a slice of parquet.Value. To reduce allocations, we preallocate
	// each row's Value slice to the schema's column count (if available).
	var buffer = make([]parquet.Row, bufferSize)
	if rrWithSchema, ok := src.(parquet.RowReaderWithSchema); ok {
		// If source schema is known, preallocate each row with capacity for all columns.
		numCols := len(rrWithSchema.Schema().Columns())
		for i := range buffer {
			buffer[i] = make([]parquet.Value, 0, numCols)
		}
	} else {
		// Initialize each row as an empty slice for the reader to fill.
		for i := range buffer {
			buffer[i] = nil
		}
	}

	currentGroupCount := 0

	// Outer loop: process one row group at a time
	for {
		currentGroupCount = 0
		// Inner loop: read until we reach rowGroupNumCount or run out of rows
		for currentGroupCount < rowGroupNumCount {
			// Limit the read batch to whichever is smaller: the buffer size or the remaining rows needed for this group.
			rowsNeeded := rowGroupNumCount - currentGroupCount
			batchSize := bufferSize
			if rowsNeeded < batchSize {
				batchSize = rowsNeeded
			}

			// Read up to batchSize rows from src into the buffer.
			n, readErr := src.ReadRows(buffer[:batchSize])
			if readErr != nil && readErr != io.EOF {
				// Abort on a hard error
				err = readErr
				return
			}
			if n == 0 {
				// No more rows to read (source exhausted)
				break
			}

			// Write the rows that were read to the destination.
			// Slice the buffer to the actual number of rows read (n).
			rowsChunk := buffer[:n]
			written, writeErr := dst.WriteRows(rowsChunk)
			if writeErr != nil {
				err = writeErr
				return
			}
			total += uint64(written)
			currentGroupCount += written

			// If we've reached EOF, break to flush the partial group.
			if readErr == io.EOF {
				break
			}
			// If this row group is full, break to flush it.
			if currentGroupCount >= rowGroupNumCount {
				break
			}
			// Otherwise, loop to read more rows for this group.
		}

		if currentGroupCount == 0 {
			// No rows read in this iteration (source fully exhausted), exit outer loop.
			break
		}

		// Flush the current row group to output (finalizing it in the Parquet file).
		flushErr := dst.Flush()
		if flushErr != nil {
			err = flushErr
			return
		}
		rowGroupCount++

		// If we ended due to EOF and the current group is smaller than the target count,
		// that was the last group; break out of outer loop.
		if currentGroupCount < rowGroupNumCount {
			break
		}
		// Otherwise, continue to process the next group.
	}

	return
}
