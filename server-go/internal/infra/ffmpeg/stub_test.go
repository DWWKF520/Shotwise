package ffmpeg

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// 测试进程可被复制成 PATH 上的 ffmpeg/ffprobe 桩：以桩身份启动时按环境变量
// 输出预设内容后退出，主进程照常跑测试。这样在未装 ffmpeg 的机器上也能覆盖
// argv 形状、退出码分类与产物校验。
const (
	stubModeEnv        = "SHOTWISE_TEST_FFMPEG_STUB"
	stubStdoutEnv      = "SHOTWISE_TEST_FFMPEG_STUB_STDOUT"
	stubStderrEnv      = "SHOTWISE_TEST_FFMPEG_STUB_STDERR"
	stubWriteEnv       = "SHOTWISE_TEST_FFMPEG_STUB_WRITE"
	stubRepeatEnv      = "SHOTWISE_TEST_FFMPEG_STUB_REPEAT"
	stubSleepMsEnv     = "SHOTWISE_TEST_FFMPEG_STUB_SLEEP_MS"
	stubExitEnv        = "SHOTWISE_TEST_FFMPEG_STUB_EXIT"
	stubArgvEnv        = "SHOTWISE_TEST_FFMPEG_STUB_ARGV"
	stubStreamsEnv     = "SHOTWISE_TEST_FFMPEG_STUB_STREAMS"
	stubDurationEnv    = "SHOTWISE_TEST_FFMPEG_STUB_DURATION"
	stubFramesEnv      = "SHOTWISE_TEST_FFMPEG_STUB_FRAMES"
	stubExactFramesEnv = "SHOTWISE_TEST_FFMPEG_STUB_EXACT_FRAMES"
	stubClipProbeEnv   = "SHOTWISE_TEST_FFMPEG_STUB_CLIP_PROBE"
	stubOutputProbeEnv = "SHOTWISE_TEST_FFMPEG_STUB_OUTPUT_PROBE"
	stubModeEcho       = "echo"
	stubModeMedia      = "media"
	stubModeProbe      = "probe"
	stubBytes          = "stub-output"
	stubWriteLatest    = "last"
)

func TestMain(m *testing.M) {
	if mode := os.Getenv(stubModeEnv); mode != "" {
		os.Exit(runStub(mode))
	}
	code := m.Run()
	if stubDirPath != "" {
		_ = os.RemoveAll(stubDirPath)
	}
	os.Exit(code)
}

// runStub 是桩侧入口：不跑测试，只按模式回放预设行为。
func runStub(mode string) int {
	switch mode {
	case stubModeMedia:
		return runMediaStub()
	case stubModeProbe:
		return runProbeStub()
	default:
		return runEchoStub()
	}
}

// runProbeStub 按 ffprobe 的探测种类作答：音频流确认返回流的类型集合、帧数探测
// 按是否全量解码分别作答、时长探测返回容器时长；带上写文件开关时同时模拟抽帧产物。
func runProbeStub() int {
	recordStubArgv()
	joined := strings.Join(os.Args, " ")
	switch {
	case strings.Contains(joined, "-select_streams a"):
		fmt.Print(envOrDefault(stubStreamsEnv, "audio"))
	case strings.Contains(joined, "-count_frames"):
		fmt.Print(envOrDefault(stubExactFramesEnv, "5"))
	case strings.Contains(joined, "stream=nb_frames"):
		fmt.Print(envOrDefault(stubFramesEnv, "2"))
	default:
		fmt.Print(envOrDefault(stubDurationEnv, "3.5"))
	}
	// 只在抽帧/缩略图这类真正写产物的调用上写文件，不碰被探测的输入。
	if strings.Contains(joined, "-frames:v") || strings.Contains(joined, "-vframes") {
		writeStubOutput()
	}
	return 0
}

// runMediaStub 扮演渲染流水线里的 ffmpeg/ffprobe：按 argv 内容区分素材探测、
// 产物校验与写出产物的渲染调用（拼接、烧录、包装、demuxer concat）。
func runMediaStub() int {
	joined := strings.Join(os.Args, " ")
	switch {
	case strings.Contains(joined, "stream=codec_type,width,height"):
		fmt.Print(envOrDefault(stubOutputProbeEnv, stubOutputProbeJSON))
		return 0
	case strings.Contains(joined, "format=duration:stream=codec_type"):
		fmt.Print(envOrDefault(stubClipProbeEnv, stubClipProbeJSON))
		return 0
	default:
		// 渲染/拼接/烧录/包装调用：记录 argv、按需回放 stderr 与休眠，再写产物。
		recordStubArgv()
		if errOut := os.Getenv(stubStderrEnv); errOut != "" {
			fmt.Fprint(os.Stderr, errOut)
		}
		writeStubOutput()
		if sleepMs := os.Getenv(stubSleepMsEnv); sleepMs != "" {
			if ms, err := strconv.Atoi(sleepMs); err == nil {
				time.Sleep(time.Duration(ms) * time.Millisecond)
			}
		}
		code, _ := strconv.Atoi(os.Getenv(stubExitEnv))
		return code
	}
}

const (
	stubClipProbeJSON   = `{"format":{"duration":"3.000000"},"streams":[{"codec_type":"audio"}]}`
	stubOutputProbeJSON = `{"format":{"duration":"3.000000"},` +
		`"streams":[{"codec_type":"video","width":480,"height":854},{"codec_type":"audio"}]}`
)

func envOrDefault(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

// runEchoStub 通用桩：回显环境变量指定的输出、可选写产物文件、可选休眠与退出码。
func runEchoStub() int {
	recordStubArgv()
	if repeat := os.Getenv(stubRepeatEnv); repeat != "" {
		if n, err := strconv.Atoi(repeat); err == nil && n > 0 {
			// 单次大写入：父进程若不持续排空管道，这里会写满管道阻塞。
			fmt.Print(strings.Repeat("o", n))
			fmt.Fprint(os.Stderr, strings.Repeat("e", n))
		}
	}
	if out := os.Getenv(stubStdoutEnv); out != "" {
		fmt.Print(out)
	}
	if errOut := os.Getenv(stubStderrEnv); errOut != "" {
		fmt.Fprint(os.Stderr, errOut)
	}
	writeStubOutput()
	// 休眠放在输出之后：运行中的调用方能在父进程轮询期间读到已写出的进度。
	if sleepMs := os.Getenv(stubSleepMsEnv); sleepMs != "" {
		if ms, err := strconv.Atoi(sleepMs); err == nil {
			time.Sleep(time.Duration(ms) * time.Millisecond)
		}
	}
	code, _ := strconv.Atoi(os.Getenv(stubExitEnv))
	return code
}

// writeStubOutput 按 SHOTWISE_TEST_FFMPEG_STUB_WRITE 写产物文件；
// 值为 last 时写 argv 最后一个参数（即工具要求写出的目标路径）。
func writeStubOutput() {
	target := os.Getenv(stubWriteEnv)
	if target == "" {
		return
	}
	if target == stubWriteLatest {
		target = os.Args[len(os.Args)-1]
	}
	if err := os.WriteFile(target, []byte(stubBytes), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "stub write failed: %v\n", err)
	}
}

func recordStubArgv() {
	path := os.Getenv(stubArgvEnv)
	if path == "" {
		return
	}
	_ = os.WriteFile(path, []byte(strings.Join(os.Args[1:], "\x00")), 0o644)
}

var (
	stubDirOnce sync.Once
	stubDirPath string
	stubDirErr  error
)

// stubBinDir 建一次桩目录：把当前测试二进制复制成 ffmpeg/ffprobe 供 PATH 查找，
// 多次安装复用同一份拷贝。
func stubBinDir(t *testing.T) string {
	t.Helper()
	stubDirOnce.Do(func() {
		self, err := os.Executable()
		if err != nil {
			stubDirErr = err
			return
		}
		dir, err := os.MkdirTemp("", "shotwise-ffmpeg-stub")
		if err != nil {
			stubDirErr = err
			return
		}
		stubDirPath = dir
		for _, name := range []string{"ffmpeg", "ffprobe"} {
			if err := copyExecutable(self, filepath.Join(dir, stubBinaryName(name))); err != nil {
				stubDirErr = err
				return
			}
		}
	})
	if stubDirErr != nil {
		t.Fatalf("install stub binaries: %v", stubDirErr)
	}
	return stubDirPath
}

// stubBinaryName 返回桩在 PATH 上的文件名（Windows 需要 .exe 后缀）。
func stubBinaryName(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}

func copyExecutable(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// installStub 把桩目录挂到 PATH 头部并清可用性缓存。
func installStub(t *testing.T) string {
	t.Helper()
	dir := stubBinDir(t)
	// 注册顺序保证 LIFO 收尾时先还原 PATH、再清缓存，避免缓存被还原后的状态污染。
	t.Cleanup(resetAvailabilityForTests)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	resetAvailabilityForTests()
	return dir
}

// removeTools 模拟未安装 ffmpeg/ffprobe 的精简环境。
func removeTools(t *testing.T) {
	t.Helper()
	t.Cleanup(resetAvailabilityForTests)
	t.Setenv("PATH", t.TempDir())
	resetAvailabilityForTests()
}

// stubEnv 设置一组桩环境变量。
func stubEnv(t *testing.T, env map[string]string) {
	t.Helper()
	for key, value := range env {
		t.Setenv(key, value)
	}
}

// readStubArgv 读取桩记录的最后一次 argv。
func readStubArgv(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read stub argv: %v", err)
	}
	return strings.Split(string(raw), "\x00")
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func containsArg(argv []string, want string) bool {
	for _, arg := range argv {
		if arg == want {
			return true
		}
	}
	return false
}

func containsPair(argv []string, flag, value string) bool {
	for index := 0; index < len(argv)-1; index++ {
		if argv[index] == flag && argv[index+1] == value {
			return true
		}
	}
	return false
}

func argValue(argv []string, flag string) string {
	for index := 0; index < len(argv)-1; index++ {
		if argv[index] == flag {
			return argv[index+1]
		}
	}
	return ""
}
