package silk

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/rs/zerolog/log"
	"github.com/sjzar/go-lame"
	"github.com/sjzar/go-silk"

	"github.com/sjzar/chatlog/pkg/util/zstd"
)

const (
	decodedSampleRate   = 24000
	minSilkHeaderLength = 9
)

// Silk2PCM16 解码 Silk 数据，返回 16-bit PCM 采样数据及采样率。
func Silk2PCM16(data []byte) ([]int16, int, error) {
	samples, rate, err := decodeSilk(data)
	if err == nil {
		return samples, rate, nil
	}

	log.Debug().Err(err).Msg("silk decode failed, retry with normalized payload")
	normalized := normalizeSilkPayload(data)
	if bytes.Equal(normalized, data) {
		log.Error().Err(err).Msg("silk decode failed; payload unchanged after normalization")
		return nil, 0, err
	}

	samples, rate, err = decodeSilk(normalized)
	if err != nil {
		log.Error().Err(err).Msg("silk decode failed after normalization")
	}
	return samples, rate, err
}

func decodeSilk(data []byte) ([]int16, int, error) {
	payload, err := prepareSilkPayload(data)
	if err != nil {
		return nil, 0, err
	}

	sd := silk.SilkInit()
	defer sd.Close()

	pcmBytes := sd.Decode(payload)
	if len(pcmBytes) == 0 {
		return nil, 0, fmt.Errorf("silk decode failed")
	}
	if len(pcmBytes)%2 != 0 {
		return nil, 0, fmt.Errorf("invalid pcm length: %d", len(pcmBytes))
	}

	samples := make([]int16, len(pcmBytes)/2)
	for i := range samples {
		samples[i] = int16(binary.LittleEndian.Uint16(pcmBytes[2*i:]))
	}

	return samples, decodedSampleRate, nil
}

var (
	silkMagic = []byte("#!SILK")
	zstdMagic = []byte{0x28, 0xb5, 0x2f, 0xfd}
)

func prepareSilkPayload(data []byte) ([]byte, error) {
	trimmed := bytes.TrimLeft(data, "\x00\xff")
	if len(trimmed) < minSilkHeaderLength {
		return nil, fmt.Errorf("silk payload too short: %d bytes", len(trimmed))
	}
	if !bytes.HasPrefix(trimmed, silkMagic) {
		return nil, fmt.Errorf("silk header missing")
	}
	return trimmed, nil
}

func normalizeSilkPayload(data []byte) []byte {
	current := data
	for i := 0; i < 3; i++ {
		trimmed := bytes.TrimLeft(current, "\x00\xff")
		if idx := bytes.Index(trimmed, silkMagic); idx >= 0 {
			return trimmed[idx:]
		}

		if next, ok := tryDecompress(trimmed); ok {
			current = next
			continue
		}

		if !bytes.Equal(trimmed, current) {
			current = trimmed
			continue
		}

		break
	}
	return current
}

type decompressor struct {
	name  string
	match func([]byte) bool
	fn    func([]byte) ([]byte, error)
}

var decompressors = []decompressor{
	{
		name: "zstd",
		match: func(b []byte) bool {
			return len(b) >= 4 && bytes.Equal(b[:4], zstdMagic)
		},
		fn: func(b []byte) ([]byte, error) {
			return zstd.Decompress(b)
		},
	},
	{
		name: "gzip",
		match: func(b []byte) bool {
			return len(b) >= 2 && b[0] == 0x1f && b[1] == 0x8b
		},
		fn: func(b []byte) ([]byte, error) {
			reader, err := gzip.NewReader(bytes.NewReader(b))
			if err != nil {
				return nil, err
			}
			defer reader.Close()
			return io.ReadAll(reader)
		},
	},
	{
		name: "zlib",
		match: func(b []byte) bool {
			return len(b) >= 2 && b[0] == 0x78
		},
		fn: func(b []byte) ([]byte, error) {
			reader, err := zlib.NewReader(bytes.NewReader(b))
			if err != nil {
				return nil, err
			}
			defer reader.Close()
			return io.ReadAll(reader)
		},
	},
}

func tryDecompress(data []byte) ([]byte, bool) {
	for _, dc := range decompressors {
		if !dc.match(data) {
			continue
		}
		out, err := dc.fn(data)
		if err != nil {
			log.Debug().Str("codec", dc.name).Err(err).Msg("silk payload decompress failed")
			continue
		}
		log.Debug().Str("codec", dc.name).Msg("silk payload decompressed")
		return out, true
	}
	return nil, false
}

func Silk2MP3(data []byte) ([]byte, error) {

	sd := silk.SilkInit()
	defer sd.Close()

	pcmdata, _, err := Silk2PCM16(data)
	if err != nil {
		return nil, err
	}

	le := lame.Init()
	defer le.Close()

	le.SetInSamplerate(24000)
	le.SetOutSamplerate(24000)
	le.SetNumChannels(1)
	le.SetBitrate(16)
	// IMPORTANT!
	le.InitParams()

	// go-lame 期望的是小端 PCM 字节序列
	pcmBytes := make([]byte, len(pcmdata)*2)
	for i, sample := range pcmdata {
		binary.LittleEndian.PutUint16(pcmBytes[i*2:], uint16(sample))
	}
	mp3data := le.Encode(pcmBytes)
	if len(mp3data) == 0 {
		return nil, fmt.Errorf("mp3 encode failed")
	}

	return mp3data, nil
}
