package model

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"math"
)

const (
	segmentMagic      = 0x52564653
	segmentVersion    = 1
	segmentHeaderSize = 64
	segmentAlignment  = 64
	segmentVector     = 0x01
	segmentManifest   = 0x05
	segmentQuant      = 0x06
	segmentMetadata   = 0x07
	segmentWitness    = 0x0a
	segmentProfile    = 0x0b
	segmentEmbedding  = 0x0c
	segmentLoRA       = 0x0d
)

var errInvalidRVF = errors.New("invalid RVF container")

type Segment struct {
	ID      uint64
	Type    uint8
	Payload []byte
}

// Container parses the RVF v1 segment format used by the existing Rust server.
// Payload slices reference the input buffer and are never copied.
type Container struct {
	data     []byte
	Segments []Segment
}

func ParseRVF(data []byte) (*Container, error) {
	if len(data) == 0 || len(data)%segmentAlignment != 0 {
		return nil, fmt.Errorf("%w: file length must be a non-zero multiple of %d", errInvalidRVF, segmentAlignment)
	}
	container := &Container{data: data}
	for offset := 0; offset < len(data); {
		if len(data)-offset < segmentHeaderSize {
			return nil, fmt.Errorf("%w: truncated segment header at offset %d", errInvalidRVF, offset)
		}
		header := data[offset : offset+segmentHeaderSize]
		if binary.LittleEndian.Uint32(header[0:4]) != segmentMagic {
			return nil, fmt.Errorf("%w: bad segment magic at offset %d", errInvalidRVF, offset)
		}
		if header[4] != segmentVersion {
			return nil, fmt.Errorf("%w: unsupported segment version %d", errInvalidRVF, header[4])
		}
		payloadLength := binary.LittleEndian.Uint64(header[16:24])
		if payloadLength > uint64(len(data)-offset-segmentHeaderSize) {
			return nil, fmt.Errorf("%w: truncated segment payload at offset %d", errInvalidRVF, offset)
		}
		payloadStart := offset + segmentHeaderSize
		payloadEnd := payloadStart + int(payloadLength)
		payload := data[payloadStart:payloadEnd]
		crc := crc32.ChecksumIEEE(payload)
		if binary.LittleEndian.Uint32(header[40:44]) != crc {
			return nil, fmt.Errorf("%w: content hash mismatch at segment %d", errInvalidRVF, binary.LittleEndian.Uint64(header[8:16]))
		}
		if !allZero(header[44:56]) {
			return nil, fmt.Errorf("%w: unsupported non-CRC content hash at segment %d", errInvalidRVF, binary.LittleEndian.Uint64(header[8:16]))
		}
		container.Segments = append(container.Segments, Segment{
			ID: binary.LittleEndian.Uint64(header[8:16]), Type: header[5], Payload: payload,
		})
		if len(container.Segments) > 65536 {
			return nil, fmt.Errorf("%w: segment count exceeds limit", errInvalidRVF)
		}
		alignedSize := align(segmentHeaderSize + int(payloadLength))
		if alignedSize > len(data)-offset {
			return nil, fmt.Errorf("%w: invalid segment padding at offset %d", errInvalidRVF, offset)
		}
		offset += alignedSize
	}
	if len(container.Segments) == 0 {
		return nil, errInvalidRVF
	}
	return container, nil
}

func (c *Container) Find(segmentType uint8) []byte {
	if c == nil {
		return nil
	}
	for _, segment := range c.Segments {
		if segment.Type == segmentType {
			return segment.Payload
		}
	}
	return nil
}

func (c *Container) Manifest() map[string]any     { return parseObject(c.Find(segmentManifest)) }
func (c *Container) Metadata() map[string]any     { return parseObject(c.Find(segmentMetadata)) }
func (c *Container) Quantization() map[string]any { return parseObject(c.Find(segmentQuant)) }
func (c *Container) Witness() map[string]any      { return parseObject(c.Find(segmentWitness)) }

func (c *Container) Weights() ([]float32, error) {
	if err := c.ValidateWeights(); err != nil {
		return nil, err
	}
	data := c.Find(segmentVector)
	weights := make([]float32, len(data)/4)
	for index, offset := 0, 0; offset < len(data); index, offset = index+1, offset+4 {
		weights[index] = math.Float32frombits(binary.LittleEndian.Uint32(data[offset : offset+4]))
	}
	return weights, nil
}

// ValidateWeights checks the full float32 vector without allocating a second
// model-sized slice. Registry scans and activation use it to keep peak memory
// bounded by the RVF container size.
func (c *Container) ValidateWeights() error {
	data := c.Find(segmentVector)
	if len(data) == 0 {
		return errors.New("model has no weight segment")
	}
	if len(data)%4 != 0 {
		return errors.New("model weight segment is not aligned to float32")
	}
	for offset := 0; offset < len(data); offset += 4 {
		value := math.Float32frombits(binary.LittleEndian.Uint32(data[offset : offset+4]))
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			return fmt.Errorf("model contains a non-finite weight at index %d", offset/4)
		}
	}
	return nil
}

func (c *Container) WeightCount() int {
	data := c.Find(segmentVector)
	if len(data)%4 != 0 {
		return 0
	}
	return len(data) / 4
}

func (c *Container) LoRAProfiles() []string {
	profiles := make([]string, 0)
	for _, segment := range c.Segments {
		if segment.Type != segmentLoRA || len(segment.Payload) < 2 {
			continue
		}
		nameLength := int(binary.LittleEndian.Uint16(segment.Payload[:2]))
		if nameLength == 0 || 2+nameLength > len(segment.Payload) {
			continue
		}
		profiles = append(profiles, string(segment.Payload[2:2+nameLength]))
	}
	return profiles
}

type ContainerInfo struct {
	SegmentCount   int            `json:"segment_count"`
	TotalSize      int            `json:"total_size"`
	Manifest       map[string]any `json:"manifest,omitempty"`
	Metadata       map[string]any `json:"metadata,omitempty"`
	HasWeights     bool           `json:"has_weights"`
	HasVitalConfig bool           `json:"has_vital_config"`
	HasQuantInfo   bool           `json:"has_quant_info"`
	HasWitness     bool           `json:"has_witness"`
}

func (c *Container) Info() ContainerInfo {
	return ContainerInfo{
		SegmentCount: len(c.Segments), TotalSize: len(c.data), Manifest: c.Manifest(), Metadata: c.Metadata(),
		HasWeights: len(c.Find(segmentVector)) != 0, HasVitalConfig: len(c.Find(segmentProfile)) != 0,
		HasQuantInfo: len(c.Find(segmentQuant)) != 0, HasWitness: len(c.Find(segmentWitness)) != 0,
	}
}

func parseObject(data []byte) map[string]any {
	if len(data) == 0 {
		return nil
	}
	var result map[string]any
	if json.Unmarshal(data, &result) != nil {
		return nil
	}
	return result
}

func align(size int) int { return (size + segmentAlignment - 1) &^ (segmentAlignment - 1) }

func allZero(data []byte) bool {
	for _, value := range data {
		if value != 0 {
			return false
		}
	}
	return true
}

// BuildRVF emits the subset of RVF v1 required by model conversion and tests.
func BuildRVF(segments ...Segment) []byte {
	var output []byte
	for index, segment := range segments {
		header := make([]byte, segmentHeaderSize)
		binary.LittleEndian.PutUint32(header[0:4], segmentMagic)
		header[4], header[5] = segmentVersion, segment.Type
		id := segment.ID
		if id == 0 {
			// RvfBuilder in the Rust implementation starts segment IDs at zero.
			// Keep the same default sequence for generated containers.
			id = uint64(index)
		}
		binary.LittleEndian.PutUint64(header[8:16], id)
		binary.LittleEndian.PutUint64(header[16:24], uint64(len(segment.Payload)))
		binary.LittleEndian.PutUint64(header[24:32], uint64(1))
		binary.LittleEndian.PutUint32(header[40:44], crc32.ChecksumIEEE(segment.Payload))
		output = append(output, header...)
		output = append(output, segment.Payload...)
		output = append(output, make([]byte, align(segmentHeaderSize+len(segment.Payload))-(segmentHeaderSize+len(segment.Payload)))...)
	}
	return output
}
