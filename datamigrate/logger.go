// datamigrate/logger.go
package datamigrate

import (
	"fmt"
	"time"
)

const (
	PrefixInfo  = "[INFO] "
	PrefixDDL   = "[DDL]  "
	PrefixData  = "[DATA] "
	PrefixIndex = "[INDEX]"
	PrefixWarn  = "[WARN] "
	PrefixError = "[ERROR]"
	PrefixDone  = "[DONE] "
)

// Logger 向 channel 写入带前缀的日志行，channel 满时丢弃（非阻塞）
type Logger struct {
	ch     chan string
	onDrop func()
}

// NewLogger 创建一个使用给定 channel 的 Logger
func NewLogger(ch chan string) *Logger {
	return &Logger{ch: ch}
}

// newCountingLogger 创建一个保持非阻塞行为、并在丢弃日志时回调的 Logger。
// NewLogger 的既有调用不受影响；迁移任务通过该构造器把计数记录到 Job。
func newCountingLogger(ch chan string, onDrop func()) *Logger {
	return &Logger{ch: ch, onDrop: onDrop}
}

func (l *Logger) Info(msg string)  { l.send(PrefixInfo + "  " + msg) }
func (l *Logger) DDL(msg string)   { l.send(PrefixDDL + "   " + msg) }
func (l *Logger) Data(msg string)  { l.send(PrefixData + "  " + msg) }
func (l *Logger) Index(msg string) { l.send(PrefixIndex + " " + msg) }
func (l *Logger) Warn(msg string)  { l.send(PrefixWarn + "  " + msg) }
func (l *Logger) Error(msg string) { l.send(PrefixError + " " + msg) }
func (l *Logger) Done(msg string)  { l.send(PrefixDone + "  " + msg) }

func (l *Logger) Infof(format string, args ...interface{})  { l.Info(fmt.Sprintf(format, args...)) }
func (l *Logger) DDLf(format string, args ...interface{})   { l.DDL(fmt.Sprintf(format, args...)) }
func (l *Logger) Dataf(format string, args ...interface{})  { l.Data(fmt.Sprintf(format, args...)) }
func (l *Logger) Indexf(format string, args ...interface{}) { l.Index(fmt.Sprintf(format, args...)) }
func (l *Logger) Warnf(format string, args ...interface{})  { l.Warn(fmt.Sprintf(format, args...)) }
func (l *Logger) Errorf(format string, args ...interface{}) { l.Error(fmt.Sprintf(format, args...)) }
func (l *Logger) Donef(format string, args ...interface{})  { l.Done(fmt.Sprintf(format, args...)) }

func (l *Logger) send(msg string) {
	ts := time.Now().Format("15:04:05.000")
	select {
	case l.ch <- ts + " " + msg:
	default:
		if l.onDrop != nil {
			l.onDrop()
		}
	}
}
