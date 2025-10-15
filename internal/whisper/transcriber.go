package whisper

import (
    "bytes"
    "context"
    "encoding/binary"
    "encoding/json"
    "errors"
    "fmt"
    "math"
    "os"
    "os/exec"
    "path/filepath"
    "runtime"
    "strings"
    "time"

    _ "embed"

    "github.com/sjzar/chatlog/pkg/util/silk"
)

//go:embed whisper.py
var embeddedWhisperScript []byte

// PythonConfig describes how to initialise a Python based whisper backend.
type PythonConfig struct {
    ScriptDir      string
    PythonPath     string
    DefaultOptions Options
    Env            map[string]string
}

// PythonTranscriber bridges to the bundled Python whisper implementation.
type PythonTranscriber struct {
    cfg        PythonConfig
    scriptPath string
}

// NewPythonTranscriber ensures the Python helper script is available and ready.
func NewPythonTranscriber(cfg PythonConfig) (*PythonTranscriber, error) {
    if cfg.ScriptDir == "" {
        return nil, errors.New("script directory is required")
    }
    if cfg.Env == nil {
        cfg.Env = make(map[string]string)
    }

    pythonPath := cfg.PythonPath
    if pythonPath == "" {
        if envPath := os.Getenv("CHATLOG_WHISPER_PYTHON"); envPath != "" {
            pythonPath = envPath
        }
    }
    if pythonPath == "" {
        if runtime.GOOS == "windows" {
            pythonPath = "python.exe"
        } else {
            pythonPath = "python3"
        }
    }

    if err := os.MkdirAll(cfg.ScriptDir, 0o755); err != nil {
        return nil, fmt.Errorf("ensure script directory: %w", err)
    }

    scriptPath := filepath.Join(cfg.ScriptDir, "whisper.py")
    if err := ensurePythonScript(scriptPath); err != nil {
        return nil, err
    }

    cfg.PythonPath = pythonPath

    return &PythonTranscriber{
        cfg:        cfg,
        scriptPath: scriptPath,
    }, nil
}

// ScriptPath returns the path to the extracted Python helper script.
func (p *PythonTranscriber) ScriptPath() string {
    return p.scriptPath
}

// Close implements the Transcriber interface. No-op for the Python backend.
func (p *PythonTranscriber) Close() {}

// TranscribePCM transcribes raw PCM samples by first writing a WAV file.
func (p *PythonTranscriber) TranscribePCM(ctx context.Context, samples []float32, sampleRate int, opts Options) (*Result, error) {
    pcm := float32ToPCM16(samples)
    return p.transcribePCM16(ctx, pcm, sampleRate, opts)
}

// TranscribeSilk decodes Silk data and transcribes it via Python whisper.
func (p *PythonTranscriber) TranscribeSilk(ctx context.Context, silkData []byte, opts Options) (*Result, error) {
    pcm, rate, err := silk.Silk2PCM16(silkData)
    if err != nil {
        return nil, err
    }
    return p.transcribePCM16(ctx, pcm, rate, opts)
}

func (p *PythonTranscriber) transcribePCM16(ctx context.Context, samples []int16, sampleRate int, opts Options) (*Result, error) {
    if len(samples) == 0 {
        return nil, errors.New("empty audio data")
    }
    tmpFile, err := os.CreateTemp("", "chatlog-whisper-*.wav")
    if err != nil {
        return nil, fmt.Errorf("create temp wav: %w", err)
    }
    wavPath := tmpFile.Name()
    tmpFile.Close()
    defer os.Remove(wavPath)

    if err := writePCM16AsWAV(wavPath, samples, sampleRate); err != nil {
        return nil, fmt.Errorf("write wav: %w", err)
    }

    args := []string{p.scriptPath, "--wav", wavPath, "--log-level", "WARNING"}
    if opts.LanguageSet && strings.TrimSpace(opts.Language) != "" {
        args = append(args, "--language", strings.TrimSpace(opts.Language))
    }
    if opts.TranslateSet && opts.Translate {
        args = append(args, "--translate")
    }

    cmd := exec.CommandContext(ctx, p.cfg.PythonPath, args...)
    env := append([]string{}, os.Environ()...)
    env = append(env, "PYTHONIOENCODING=utf-8")
    for key, value := range p.cfg.Env {
        env = append(env, fmt.Sprintf("%s=%s", key, value))
    }
    cmd.Env = env

    output, err := cmd.CombinedOutput()
    if err != nil {
        if ctx.Err() != nil {
            return nil, ctx.Err()
        }
        return nil, fmt.Errorf("python whisper: %w: %s", err, strings.TrimSpace(string(output)))
    }

    var resp pythonResult
    if err := json.Unmarshal(bytes.TrimSpace(output), &resp); err != nil {
        return nil, fmt.Errorf("decode python whisper response: %w", err)
    }
    if resp.Error != "" {
        return nil, errors.New(resp.Error)
    }

    result := &Result{
        Text:     resp.Text,
        Language: resp.Language,
        Duration: pcmDuration(len(samples), sampleRate),
    }
    if result.Language == "" && opts.LanguageSet {
        result.Language = opts.Language
    }

    if len(resp.Segments) > 0 {
        segments := make([]Segment, 0, len(resp.Segments))
        for _, seg := range resp.Segments {
            segments = append(segments, Segment{
                ID:    seg.ID,
                Start: secondsToDuration(seg.Start),
                End:   secondsToDuration(seg.End),
                Text:  seg.Text,
            })
        }
        result.Segments = segments
    }

    return result, nil
}

func ensurePythonScript(path string) error {
    if info, err := os.Stat(path); err == nil && !info.IsDir() {
        current, readErr := os.ReadFile(path)
        if readErr == nil && bytes.Equal(current, embeddedWhisperScript) {
            return nil
        }
    }
    if err := os.WriteFile(path, embeddedWhisperScript, 0o644); err != nil {
        return fmt.Errorf("write whisper helper: %w", err)
    }
    return nil
}

func float32ToPCM16(src []float32) []int16 {
    if len(src) == 0 {
        return nil
    }
    dst := make([]int16, len(src))
    for i, sample := range src {
        v := float64(sample)
        if v > 1 {
            v = 1
        } else if v < -1 {
            v = -1
        }
        dst[i] = int16(math.Round(v * 32767))
    }
    return dst
}

func writePCM16AsWAV(path string, samples []int16, sampleRate int) error {
    if sampleRate <= 0 {
        sampleRate = 24000
    }

    file, err := os.Create(path)
    if err != nil {
        return err
    }
    defer file.Close()

    dataSize := len(samples) * 2
    riffSize := 36 + dataSize
    byteRate := sampleRate * 2
    blockAlign := 2

    header := make([]byte, 44)
    copy(header[0:], []byte("RIFF"))
    binary.LittleEndian.PutUint32(header[4:], uint32(riffSize))
    copy(header[8:], []byte("WAVEfmt "))
    binary.LittleEndian.PutUint32(header[16:], 16)
    binary.LittleEndian.PutUint16(header[20:], 1)
    binary.LittleEndian.PutUint16(header[22:], 1)
    binary.LittleEndian.PutUint32(header[24:], uint32(sampleRate))
    binary.LittleEndian.PutUint32(header[28:], uint32(byteRate))
    binary.LittleEndian.PutUint16(header[32:], uint16(blockAlign))
    binary.LittleEndian.PutUint16(header[34:], 16)
    copy(header[36:], []byte("data"))
    binary.LittleEndian.PutUint32(header[40:], uint32(dataSize))

    if _, err := file.Write(header); err != nil {
        return err
    }

    payload := make([]byte, len(samples)*2)
    for i, sample := range samples {
        binary.LittleEndian.PutUint16(payload[i*2:], uint16(sample))
    }
    if _, err := file.Write(payload); err != nil {
        return err
    }

    return nil
}

func pcmDuration(sampleCount int, sampleRate int) time.Duration {
    if sampleRate <= 0 || sampleCount <= 0 {
        return 0
    }
    seconds := float64(sampleCount) / float64(sampleRate)
    return secondsToDuration(seconds)
}

func secondsToDuration(seconds float64) time.Duration {
    if math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds <= 0 {
        return 0
    }
    return time.Duration(seconds * float64(time.Second))
}

type pythonResult struct {
    Text     string          `json:"text"`
    Language string          `json:"language"`
    Segments []pythonSegment `json:"segments"`
    Error    string          `json:"error"`
}

type pythonSegment struct {
    ID    int     `json:"id"`
    Start float64 `json:"start"`
    End   float64 `json:"end"`
    Text  string  `json:"text"`
}
