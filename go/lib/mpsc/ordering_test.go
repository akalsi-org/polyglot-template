//go:build linux && (amd64 || arm64)

package mpsc

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

const orderingHeaderSize = 24

func orderingByte(producer uint32, sequence uint64, offset int) byte {
	return byte(sequence*131 + uint64(producer)*17 + uint64(offset)*29)
}

func orderingChecksum(producer uint32, sequence uint64, length int) uint64 {
	checksum := uint64(producer)<<32 ^ sequence ^ uint64(length)
	for offset := orderingHeaderSize; offset < length; offset++ {
		checksum = checksum*1099511628211 ^ uint64(orderingByte(producer, sequence, offset))
	}
	return checksum
}

func makeOrderingRecord(producer uint32, sequence uint64, length int) []byte {
	record := make([]byte, length)
	binary.LittleEndian.PutUint32(record[0:4], producer)
	binary.LittleEndian.PutUint64(record[4:12], sequence)
	binary.LittleEndian.PutUint32(record[12:16], uint32(length))
	binary.LittleEndian.PutUint64(record[16:24], orderingChecksum(producer, sequence, length))
	for offset := orderingHeaderSize; offset < length; offset++ {
		record[offset] = orderingByte(producer, sequence, offset)
	}
	return record
}

func validateOrderingRecord(record []byte, expected []uint64) error {
	if len(record) < orderingHeaderSize {
		return fmt.Errorf("short record: %d", len(record))
	}
	producer := binary.LittleEndian.Uint32(record[0:4])
	if int(producer) >= len(expected) {
		return fmt.Errorf("producer %d out of range", producer)
	}
	sequence := binary.LittleEndian.Uint64(record[4:12])
	if sequence != expected[producer] {
		return fmt.Errorf("producer %d sequence %d, want %d", producer, sequence, expected[producer])
	}
	declared := int(binary.LittleEndian.Uint32(record[12:16]))
	if declared != len(record) {
		return fmt.Errorf("producer %d sequence %d length %d, want %d", producer, sequence, len(record), declared)
	}
	checksum := binary.LittleEndian.Uint64(record[16:24])
	if want := orderingChecksum(producer, sequence, len(record)); checksum != want {
		return fmt.Errorf("producer %d sequence %d checksum %#x, want %#x", producer, sequence, checksum, want)
	}
	for offset := orderingHeaderSize; offset < len(record); offset++ {
		if want := orderingByte(producer, sequence, offset); record[offset] != want {
			return fmt.Errorf("producer %d sequence %d byte %d is %d, want %d", producer, sequence, offset, record[offset], want)
		}
	}
	expected[producer]++
	return nil
}

func TestMPSCOrderingPublicationChild(t *testing.T) {
	producerText := os.Getenv("PGT_MPSC_ORDERING_CHILD")
	if producerText == "" {
		return
	}
	producerValue, err := strconv.ParseUint(producerText, 10, 32)
	if err != nil {
		t.Fatal(err)
	}
	records, err := strconv.Atoi(os.Getenv("PGT_MPSC_ORDERING_RECORDS"))
	if err != nil || records <= 0 {
		t.Fatalf("invalid record count: %q", os.Getenv("PGT_MPSC_ORDERING_RECORDS"))
	}
	q, err := AttachMPSC(3)
	if err != nil {
		t.Fatal(err)
	}
	producer, err := q.NewProducer()
	if err != nil {
		t.Fatal(err)
	}
	fmt.Println("ready")
	if _, err := bufio.NewReader(os.Stdin).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	lengths := [...]int{24, 31, 63, 64, 65, 127, 191}
	for sequence := uint64(0); sequence < uint64(records); sequence++ {
		record := makeOrderingRecord(uint32(producerValue), sequence, lengths[sequence%uint64(len(lengths))])
		for {
			err := producer.Write(record)
			if err == nil {
				break
			}
			if !errors.Is(err, ErrFull) && !errors.Is(err, ErrContended) {
				t.Fatal(err)
			}
			runtime.Gosched()
		}
	}
	if err := producer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := q.Close(); err != nil {
		t.Fatal(err)
	}
	fmt.Println("done")
}

type orderingChild struct {
	cmd   *exec.Cmd
	input *bufio.Writer
	out   *bufio.Reader
}

func startOrderingChild(t *testing.T, q *MPSC, producer, records int) *orderingChild {
	t.Helper()
	fd, err := q.DupFD()
	if err != nil {
		t.Fatal(err)
	}
	file := os.NewFile(uintptr(fd), "mpsc-ordering-region")
	cmd := exec.Command(os.Args[0], "-test.run=TestMPSCOrderingPublicationChild$")
	cmd.ExtraFiles = []*os.File{file}
	cmd.Env = append(os.Environ(),
		"PGT_MPSC_ORDERING_CHILD="+strconv.Itoa(producer),
		"PGT_MPSC_ORDERING_RECORDS="+strconv.Itoa(records),
	)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		file.Close()
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		file.Close()
		t.Fatal(err)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		file.Close()
		t.Fatal(err)
	}
	file.Close()
	child := &orderingChild{cmd: cmd, input: bufio.NewWriter(stdin), out: bufio.NewReader(stdout)}
	line, err := child.out.ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "ready" {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("producer %d did not become ready: %q %v %s", producer, line, err, stderr.String())
	}
	return child
}

func orderingRecordCount(t *testing.T) int {
	t.Helper()
	text := os.Getenv("PGT_MPSC_ORDERING_RECORDS")
	if text == "" {
		return 5000
	}
	count, err := strconv.Atoi(text)
	if err != nil || count <= 0 {
		t.Fatalf("invalid PGT_MPSC_ORDERING_RECORDS: %q", text)
	}
	return count
}

func TestMPSCReleaseAcquirePublication(t *testing.T) {
	const writers = 4
	records := orderingRecordCount(t)
	q, err := CreateMPSC(Config{Capacity: 4096, MPSCLayout: MPSCCompact})
	if err != nil {
		t.Fatal(err)
	}
	consumer, err := q.AttachConsumer()
	if err != nil {
		t.Fatal(err)
	}
	children := make([]*orderingChild, 0, writers)
	defer func() {
		for _, child := range children {
			if child.cmd.ProcessState == nil {
				_ = child.cmd.Process.Kill()
				_ = child.cmd.Wait()
			}
		}
	}()
	for producer := range writers {
		children = append(children, startOrderingChild(t, q, producer, records))
	}
	for _, child := range children {
		if _, err := child.input.WriteString("go\n"); err != nil {
			t.Fatal(err)
		}
		if err := child.input.Flush(); err != nil {
			t.Fatal(err)
		}
	}

	expected := make([]uint64, writers)
	remaining := writers * records
	progressDeadline := time.Now().Add(10 * time.Second)
	for remaining != 0 {
		record, ok, err := consumer.Peek()
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			if time.Now().After(progressDeadline) {
				t.Fatalf("no progress; remaining=%d sequences=%v", remaining, expected)
			}
			runtime.Gosched()
			continue
		}
		if err := validateOrderingRecord(record.UnsafeBytes(), expected); err != nil {
			t.Fatal(err)
		}
		if err := consumer.Pop(); err != nil {
			t.Fatal(err)
		}
		remaining--
		progressDeadline = time.Now().Add(10 * time.Second)
	}
	for producer, child := range children {
		line, err := child.out.ReadString('\n')
		if err != nil || strings.TrimSpace(line) != "done" {
			t.Fatalf("producer %d completion: %q %v", producer, line, err)
		}
		if err := child.cmd.Wait(); err != nil {
			t.Fatalf("producer %d exit: %v", producer, err)
		}
	}
	if err := consumer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := q.Close(); err != nil {
		t.Fatal(err)
	}
}
