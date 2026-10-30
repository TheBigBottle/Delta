package manager

// Captures record the data a forwarder receives into a file, with timestamps,
// so it can be downloaded or replayed later into any running forwarder.
//
// .dcap file format: the line "DCAP1\n", followed by records of
//
//	int64  time offset in nanoseconds since the capture started
//	uint16 length of the source address
//	       source address (e.g. "10.0.0.5:4001")
//	uint32 length of the data
//	       data
//
// All numbers are big endian.

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"delta/internal/engine"
)

const dcapMagic = "DCAP1\n"

var (
	// backgroundJobs tracks running captures and replays, so shutdown can
	// wait until capture files are completely written.
	backgroundJobs sync.WaitGroup

	validCaptureName = regexp.MustCompile(`^[A-Za-z0-9._-]+\.dcap$`)
	unsafeFileChars  = regexp.MustCompile(`[^A-Za-z0-9_-]+`)
)

// safeFileName turns a forwarder or upload name into something usable in a file name.
func safeFileName(name, fallback string) string {
	safe := strings.Trim(unsafeFileChars.ReplaceAllString(name, "-"), "-")
	if safe == "" {
		return fallback
	}
	return safe
}

type CaptureStatus struct {
	FileName  string    `json:"name"`
	StartedAt time.Time `json:"started"`
	Records   int64     `json:"records"`
	Bytes     int64     `json:"bytes"`
}

// captureJob is a running recording.
type captureJob struct {
	fileName    string
	startedAt   time.Time
	recordCount atomic.Int64
	byteCount   atomic.Int64
	stopSignal  chan struct{}
	stopOnce    sync.Once
}

func (job *captureJob) stop() { job.stopOnce.Do(func() { close(job.stopSignal) }) }

func (job *captureJob) status() CaptureStatus {
	return CaptureStatus{FileName: job.fileName, StartedAt: job.startedAt, Records: job.recordCount.Load(), Bytes: job.byteCount.Load()}
}

func (m *Manager) captureDir() string { return filepath.Join(m.dataDir, "captures") }

// StartCapture records everything the forwarder's input receives until the
// capture is stopped, the forwarder stops, or one of the limits is reached
// (0 = no limit).
func (m *Manager) StartCapture(id string, maxSeconds int, maxMegabytes int) (CaptureStatus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	inst := m.instances[id]
	switch {
	case inst == nil:
		return CaptureStatus{}, ErrNotFound
	case inst.running == nil:
		return CaptureStatus{}, errors.New("forwarder is not running")
	case inst.capture != nil:
		return CaptureStatus{}, errors.New("a capture is already running")
	}
	if err := os.MkdirAll(m.captureDir(), 0o750); err != nil {
		return CaptureStatus{}, err
	}
	startedAt := time.Now()
	fileName := fmt.Sprintf("%s-%s.dcap", safeFileName(inst.config.Name, "capture"), startedAt.Format("20060102-150405"))
	file, err := os.OpenFile(filepath.Join(m.captureDir(), fileName), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640)
	if err != nil {
		return CaptureStatus{}, err
	}

	job := &captureJob{fileName: fileName, startedAt: startedAt, stopSignal: make(chan struct{})}
	inst.capture = job
	fw := inst.running
	// A large buffer so short bursts are not lost while the disk is busy.
	tapped := fw.Subscribe(65536)
	m.Events.Add("info", id, inst.config.Name, "capture started: "+fileName)

	var timeLimit <-chan time.Time // nil channel = no time limit
	if maxSeconds > 0 {
		timeLimit = time.After(time.Duration(maxSeconds) * time.Second)
	}
	maxBytes := int64(maxMegabytes) << 20

	backgroundJobs.Go(func() {
		defer fw.Unsubscribe(tapped)
		writer := bufio.NewWriterSize(file, 256<<10)
		writer.WriteString(dcapMagic)
		startNanos := startedAt.UnixNano()
		flushTicker := time.NewTicker(time.Second) // keep the file reasonably current
		defer flushTicker.Stop()

		var writeErr error
	recording:
		for {
			select {
			case message := <-tapped:
				if writeErr = writeRecord(writer, message.Time-startNanos, message.Source, message.Data); writeErr != nil {
					break recording
				}
				job.recordCount.Add(1)
				if job.byteCount.Add(int64(len(message.Data))) >= maxBytes && maxBytes > 0 {
					break recording
				}
			case <-flushTicker.C:
				if writeErr = writer.Flush(); writeErr != nil {
					break recording
				}
			case <-timeLimit:
				break recording
			case <-job.stopSignal:
				break recording
			}
		}
		if err := writer.Flush(); writeErr == nil {
			writeErr = err
		}
		if err := file.Close(); writeErr == nil {
			writeErr = err
		}

		m.mu.Lock()
		if inst.capture == job {
			inst.capture = nil
		}
		m.mu.Unlock()
		if writeErr != nil {
			m.Events.Add("error", id, inst.config.Name, "capture "+fileName+" failed: "+writeErr.Error())
		} else {
			m.Events.Add("info", id, inst.config.Name, fmt.Sprintf("capture saved: %s (%d records, %d bytes)", fileName, job.recordCount.Load(), job.byteCount.Load()))
		}
		m.BroadcastForwarders()
	})
	return job.status(), nil
}

func (m *Manager) StopCapture(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	inst := m.instances[id]
	if inst == nil {
		return ErrNotFound
	}
	if inst.capture == nil {
		return errors.New("no capture running")
	}
	inst.capture.stop()
	return nil
}

// writeRecord appends one record in .dcap format.
func writeRecord(w io.Writer, offsetNanos int64, source string, data []byte) error {
	if len(source) > 0xffff {
		source = source[:0xffff]
	}
	var header [14]byte
	binary.BigEndian.PutUint64(header[0:], uint64(offsetNanos))
	binary.BigEndian.PutUint16(header[8:], uint16(len(source)))
	if _, err := w.Write(header[:10]); err != nil {
		return err
	}
	if _, err := io.WriteString(w, source); err != nil {
		return err
	}
	binary.BigEndian.PutUint32(header[10:], uint32(len(data)))
	if _, err := w.Write(header[10:14]); err != nil {
		return err
	}
	_, err := w.Write(data)
	return err
}

// dcapReader reads the records of a .dcap file one by one and knows its byte
// position, so a later request can continue where the previous one stopped.
type dcapReader struct {
	reader   *bufio.Reader
	position int64 // byte position of the next record in the file
}

var errCorruptRecord = errors.New("corrupt record")

// newDcapReader checks the file header and returns a reader at the first record.
func newDcapReader(r io.Reader) (*dcapReader, error) {
	reader := bufio.NewReaderSize(r, 256<<10)
	magic := make([]byte, len(dcapMagic))
	if _, err := io.ReadFull(reader, magic); err != nil || string(magic) != dcapMagic {
		return nil, errors.New("not a .dcap file")
	}
	return &dcapReader{reader: reader, position: int64(len(dcapMagic))}, nil
}

// next returns the next record. At the end of the file it returns io.EOF;
// a truncated last record (e.g. after a crash during recording) counts as the end.
func (d *dcapReader) next() (offsetNanos int64, source string, data []byte, err error) {
	var header [10]byte
	if _, err := io.ReadFull(d.reader, header[:]); err != nil {
		return 0, "", nil, io.EOF
	}
	offsetNanos = int64(binary.BigEndian.Uint64(header[0:]))
	sourceBytes := make([]byte, binary.BigEndian.Uint16(header[8:]))
	if _, err := io.ReadFull(d.reader, sourceBytes); err != nil {
		return 0, "", nil, io.EOF
	}
	if _, err := io.ReadFull(d.reader, header[:4]); err != nil {
		return 0, "", nil, io.EOF
	}
	dataLength := binary.BigEndian.Uint32(header[:4])
	if dataLength > 64<<20 {
		return 0, "", nil, errCorruptRecord // protects against allocating huge buffers
	}
	data = make([]byte, dataLength)
	if _, err := io.ReadFull(d.reader, data); err != nil {
		return 0, "", nil, io.EOF
	}
	d.position += int64(10 + len(sourceBytes) + 4 + len(data))
	return offsetNanos, string(sourceBytes), data, nil
}

// ReadDcap calls handleRecord for every record of a .dcap file.
func ReadDcap(r io.Reader, handleRecord func(offsetNanos int64, source string, data []byte) error) error {
	records, err := newDcapReader(r)
	if err != nil {
		return err
	}
	for {
		offsetNanos, source, data, err := records.next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if err := handleRecord(offsetNanos, source, data); err != nil {
			return err
		}
	}
}

// CapturePage is one page of records for the capture viewer.
type CapturePage struct {
	Records  []InspectedMessage `json:"records"`
	Next     int64              `json:"next"`     // byte position for the next page; -1 = end of file
	Position int64              `json:"position"` // bytes of the file read so far (for a progress bar)
	FileSize int64              `json:"file_size"`
	Scanned  int                `json:"scanned"` // records looked at on this page (incl. filtered out)
}

// Limits per page request, so a search through a huge capture never blocks
// the server for long; the viewer simply asks for the next page.
const (
	maxRecordsScannedPerPage = 200_000
	maxBytesScannedPerPage   = 64 << 20
)

// ReadCapturePage reads up to limit records that match the filter (nil =
// all), starting at byte position from (0 = first record).
func (m *Manager) ReadCapturePage(name string, from int64, limit, previewBytes int, matches func([]byte) bool) (CapturePage, error) {
	path, err := m.CapturePath(name)
	if err != nil {
		return CapturePage{}, err
	}
	file, err := os.Open(path)
	if err != nil {
		return CapturePage{}, errors.New("capture not found")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return CapturePage{}, err
	}
	records, err := newDcapReader(file) // also validates the file header
	if err != nil {
		return CapturePage{}, err
	}
	if from > records.position { // continue a previous page: jump to its end
		if _, err := file.Seek(from, io.SeekStart); err != nil {
			return CapturePage{}, err
		}
		records = &dcapReader{reader: bufio.NewReaderSize(file, 256<<10), position: from}
	}

	page := CapturePage{Records: []InspectedMessage{}, FileSize: info.Size()}
	startPosition := records.position
	for len(page.Records) < limit {
		if page.Scanned >= maxRecordsScannedPerPage || records.position-startPosition >= maxBytesScannedPerPage {
			break // continue with the next request
		}
		offsetNanos, source, data, err := records.next()
		if err == io.EOF {
			page.Next, page.Position = -1, records.position
			return page, nil
		}
		if err != nil {
			return CapturePage{}, err
		}
		page.Scanned++
		if matches != nil && !matches(data) {
			continue
		}
		page.Records = append(page.Records, InspectedMessage{
			TimeMillis: float64(offsetNanos) / float64(time.Millisecond),
			Source:     source, Length: len(data), Data: PreviewOf(data, previewBytes),
		})
	}
	page.Next, page.Position = records.position, records.position
	return page, nil
}

// ParseRecordFilter builds the capture viewer's search function: "/…/flags"
// is a regular expression (flag i = ignore case), any other text a
// case-insensitive substring. An empty filter returns nil (everything matches).
func ParseRecordFilter(filter string) (func([]byte) bool, error) {
	if filter == "" {
		return nil, nil
	}
	if strings.HasPrefix(filter, "/") && strings.LastIndex(filter, "/") > 0 {
		end := strings.LastIndex(filter, "/")
		pattern, flags := filter[1:end], filter[end+1:]
		if strings.Contains(flags, "i") {
			pattern = "(?i)" + pattern
		}
		re, err := regexp.Compile(pattern)
		if err != nil {
			return nil, fmt.Errorf("invalid regular expression: %w", err)
		}
		return re.Match, nil
	}
	needle := bytes.ToLower([]byte(filter))
	return func(data []byte) bool { return bytes.Contains(bytes.ToLower(data), needle) }, nil
}

type CaptureFile struct {
	Name      string    `json:"name"`
	Size      int64     `json:"size"`
	Modified  time.Time `json:"modified"`
	Recording bool      `json:"active"` // still being written
}

// CaptureFiles lists all capture files, newest first.
func (m *Manager) CaptureFiles() ([]CaptureFile, error) {
	entries, err := os.ReadDir(m.captureDir())
	if errors.Is(err, os.ErrNotExist) {
		return []CaptureFile{}, nil
	}
	if err != nil {
		return nil, err
	}
	beingRecorded := map[string]bool{}
	m.mu.Lock()
	for _, inst := range m.instances {
		if inst.capture != nil {
			beingRecorded[inst.capture.fileName] = true
		}
	}
	m.mu.Unlock()

	files := []CaptureFile{}
	for _, entry := range entries {
		if !validCaptureName.MatchString(entry.Name()) {
			continue
		}
		if info, err := entry.Info(); err == nil {
			files = append(files, CaptureFile{Name: entry.Name(), Size: info.Size(), Modified: info.ModTime(), Recording: beingRecorded[entry.Name()]})
		}
	}
	slices.SortFunc(files, func(a, b CaptureFile) int { return b.Modified.Compare(a.Modified) })
	return files, nil
}

// CapturePath returns the full path of a capture file. The name is checked
// strictly, so a request cannot reach files outside the capture directory.
func (m *Manager) CapturePath(name string) (string, error) {
	if !validCaptureName.MatchString(name) {
		return "", errors.New("invalid capture name")
	}
	return filepath.Join(m.captureDir(), name), nil
}

func (m *Manager) DeleteCapture(name string) error {
	path, err := m.CapturePath(name)
	if err != nil {
		return err
	}
	return os.Remove(path)
}

// ImportCapture stores an uploaded file and returns its capture name. A .dcap
// file is kept as is. Any other file is converted: text (e.g. an NMEA log)
// becomes one record per line, binary data one record per 1 KiB.
func (m *Manager) ImportCapture(uploadName string, r io.Reader) (string, error) {
	data, err := io.ReadAll(io.LimitReader(r, 256<<20))
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(m.captureDir(), 0o750); err != nil {
		return "", err
	}
	baseName := safeFileName(strings.TrimSuffix(filepath.Base(uploadName), ".dcap"), "upload")
	fileName := baseName + ".dcap"
	for i := 2; ; i++ { // never overwrite an existing capture
		if _, err := os.Stat(filepath.Join(m.captureDir(), fileName)); errors.Is(err, os.ErrNotExist) {
			break
		}
		fileName = fmt.Sprintf("%s-%d.dcap", baseName, i)
	}

	var converted bytes.Buffer
	if bytes.HasPrefix(data, []byte(dcapMagic)) {
		converted.Write(data)
	} else {
		converted.WriteString(dcapMagic)
		isText := bytes.IndexByte(data, 0) < 0 && bytes.IndexByte(data, '\n') >= 0
		for len(data) > 0 {
			recordLength := min(len(data), 1024)
			if isText {
				if newline := bytes.IndexByte(data, '\n'); newline >= 0 {
					recordLength = newline + 1
				} else {
					recordLength = len(data)
				}
			}
			// Uploaded files have no timing; replay them with a fixed interval.
			writeRecord(&converted, 0, "upload", data[:recordLength])
			data = data[recordLength:]
		}
	}
	if err := os.WriteFile(filepath.Join(m.captureDir(), fileName), converted.Bytes(), 0o640); err != nil {
		return "", err
	}
	return fileName, nil
}

// ---- replay ----

type ReplayStatus struct {
	CaptureName string    `json:"capture"`
	StartedAt   time.Time `json:"started"`
	Sent        int64     `json:"sent"`
	Loop        bool      `json:"loop"`
}

type replayJob struct {
	ReplayStatus
	sentCount  atomic.Int64
	stopSignal chan struct{}
	stopOnce   sync.Once
}

func (job *replayJob) stop() { job.stopOnce.Do(func() { close(job.stopSignal) }) }

func (job *replayJob) status() ReplayStatus {
	status := job.ReplayStatus
	status.Sent = job.sentCount.Load()
	return status
}

// ReplayOptions control the timing of a replay. Exactly one timing applies:
//   - IntervalMS > 0: a fixed pause between records
//   - Speed > 0: the recorded timing, sped up by this factor (1 = real time)
//   - both 0: as fast as possible
type ReplayOptions struct {
	ForwarderID string  `json:"forwarder"`
	Speed       float64 `json:"speed"`
	IntervalMS  int     `json:"interval_ms"`
	Loop        bool    `json:"loop"`
}

// StartReplay feeds a capture into a running forwarder, as if its input had
// received the data. All of the forwarder's outputs get it.
func (m *Manager) StartReplay(captureName string, options ReplayOptions) error {
	path, err := m.CapturePath(captureName)
	if err != nil {
		return err
	}
	if _, err := os.Stat(path); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	inst := m.instances[options.ForwarderID]
	switch {
	case inst == nil:
		return ErrNotFound
	case inst.running == nil:
		return errors.New("forwarder is not running")
	case inst.replay != nil:
		return errors.New("a replay is already running on this forwarder")
	}
	job := &replayJob{
		ReplayStatus: ReplayStatus{CaptureName: captureName, StartedAt: time.Now(), Loop: options.Loop},
		stopSignal:   make(chan struct{}),
	}
	inst.replay = job
	fw := inst.running
	m.Events.Add("info", inst.config.ID, inst.config.Name, "replay started: "+captureName)

	backgroundJobs.Go(func() {
		err := replayCaptureFile(path, fw, job, options)
		m.mu.Lock()
		if inst.replay == job {
			inst.replay = nil
		}
		m.mu.Unlock()
		message := fmt.Sprintf("replay finished: %s (%d records)", captureName, job.sentCount.Load())
		if err != nil {
			message = "replay " + captureName + " failed: " + err.Error()
		}
		m.Events.Add("info", inst.config.ID, inst.config.Name, message)
		m.BroadcastForwarders()
	})
	return nil
}

var errReplayStopped = errors.New("replay stopped")

func replayCaptureFile(path string, fw *engine.Forwarder, job *replayJob, options ReplayOptions) error {
	// wait pauses for d, or returns errReplayStopped if the replay or the forwarder is stopped.
	wait := func(d time.Duration) error {
		if d <= 0 {
			return nil
		}
		timer := time.NewTimer(d)
		defer timer.Stop()
		select {
		case <-timer.C:
			return nil
		case <-job.stopSignal:
		case <-fw.Done():
		}
		return errReplayStopped
	}

	for {
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		passStartedAt, firstOffset := time.Now(), int64(-1)
		err = ReadDcap(file, func(offsetNanos int64, _ string, data []byte) error {
			select {
			case <-job.stopSignal:
				return errReplayStopped
			case <-fw.Done():
				return errReplayStopped
			default:
			}
			switch {
			case options.IntervalMS > 0:
				if job.sentCount.Load() > 0 { // no pause before the very first record
					if err := wait(time.Duration(options.IntervalMS) * time.Millisecond); err != nil {
						return err
					}
				}
			case options.Speed > 0:
				if firstOffset < 0 {
					firstOffset = offsetNanos
				}
				dueAt := passStartedAt.Add(time.Duration(float64(offsetNanos-firstOffset) / options.Speed))
				if err := wait(time.Until(dueAt)); err != nil {
					return err
				}
			}
			fw.Dispatch(data, "replay")
			job.sentCount.Add(1)
			return nil
		})
		file.Close()
		if errors.Is(err, errReplayStopped) {
			return nil
		}
		if err != nil || !options.Loop {
			return err
		}
	}
}

// StopReplay stops the replay running into a forwarder.
func (m *Manager) StopReplay(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	inst := m.instances[id]
	if inst == nil {
		return ErrNotFound
	}
	if inst.replay == nil {
		return errors.New("no replay running")
	}
	inst.replay.stop()
	return nil
}

// InspectedMessage is one row of the live inspector or the capture viewer.
type InspectedMessage struct {
	TimeMillis float64 `json:"t"` // live: unix ms; capture: ms since the capture started
	Source     string  `json:"s"`
	Length     int     `json:"n"` // full length; Data may be shortened
	Data       []byte  `json:"d"` // the first previewBytes bytes, base64 in JSON
}

// PreviewOf returns the first previewBytes bytes of data (all of it if shorter).
func PreviewOf(data []byte, previewBytes int) []byte {
	if len(data) > previewBytes {
		return data[:previewBytes]
	}
	return data
}
