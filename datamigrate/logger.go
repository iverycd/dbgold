// datamigrate/logger.go
package datamigrate

import "fmt"

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
	ch chan string
}

// NewLogger 创建一个使用给定 channel 的 Logger
func NewLogger(ch chan string) *Logger {
	return &Logger{ch: ch}
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
	select {
	case l.ch <- msg:
	default:
	}
}
