package silk

import (
	"encoding/binary"
	"fmt"

	"github.com/sjzar/go-lame"
	"github.com/sjzar/go-silk"
)

const decodedSampleRate = 24000

// Silk2PCM16 解码 Silk 数据，返回 16-bit PCM 采样数据及采样率。
func Silk2PCM16(data []byte) ([]int16, int, error) {
	sd := silk.SilkInit()
	defer sd.Close()

	pcmBytes := sd.Decode(data)
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
