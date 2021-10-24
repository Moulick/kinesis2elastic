/*
Copyright 2018 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package log contains utilities for fetching a new logger
// when one is not already available.
//
// The Log Handle
//
// This package contains a root logr.Logger Log.  It may be used to
// get a handle to whatever the root logging implementation is.  By
// default, no implementation exists, and the handle returns "promises"
// to loggers.  When the implementation is set using SetLogger, these
// "promises" will be converted over to real loggers.
//
// Logr
//
// All logging in controller-runtime is structured, using a set of interfaces
// defined by a package called logr
// (https://godoc.org/github.com/go-logr/logr).  The sub-package zap provides
// helpers for setting up logr backed by Zap (go.uber.org/zap).
package log

import (
	"fmt"
	"strings"

	"github.com/jnovack/flag"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// Options contains all possible settings
type Options struct {
	// Development configures the logger to use a Zap development config
	// (stacktraces on warnings, no sampling), otherwise a Zap production
	// config will be used (stacktraces on errors, sampling).
	Development bool
	// Encoder configures how Zap will encode the output. Defaults to
	// console
	Encoder string
	// Level configures the verbosity of the logging. Defaults to Debug when
	// Development is true and Info otherwise
	Level int
	// Stacktrace enables stacktrace in log messages. Defaults to false
	Stacktrace bool
}

// BindFlags will parse the given flagset for zap option flags and set the log options accordingly
//  zap-devel: Development Mode defaults(encoder=consoleEncoder,logLevel=Debug,stackTraceLevel=Warn)
//			  Production Mode defaults(encoder=jsonEncoder,logLevel=Info,stackTraceLevel=Error)
//  zap-encoder: Zap log encoding (one of 'json' or 'console')
//  zap-log-level:  Zap Level to configure the verbosity of logging. Can be one of 'debug', 'info', 'error',
//			       or any integer value > 0 which corresponds to custom debug levels of increasing verbosity")
//  zap-stacktrace-level: Zap Level at and above which stacktraces are captured (one of 'info' or 'error')
func (o *Options) BindFlags(fs *flag.FlagSet) {
	// Set Development mode value
	fs.BoolVar(&o.Development, "zap-devel", false,
		"Development Mode defaults(encoder=consoleEncoder,logLevel=Debug,stackTraceLevel=Warn). "+
			"Production Mode defaults(encoder=jsonEncoder,logLevel=Info,stackTraceLevel=Error)")

	// Set Encoder value
	fs.StringVar(&o.Encoder, "zap-encoder", "json", "Zap log encoding (one of 'json' or 'console')")

	// Set the Log Level
	fs.IntVar(&o.Level, "zap-log-level", 0,
		"Zap Level to configure the verbosity of logging. Can be one of 'debug', 'info', 'error', "+
			"or any integer value > 0 which corresponds to custom debug levels of increasing verbosity")

	// Set the StrackTrace
	fs.BoolVar(&o.Stacktrace, "zap-stacktrace-level", false,
		"Zap Level at and above which stacktraces are captured (one of 'info', 'error').")
}

//
// var levelStrings = map[string]zapcore.Level{
// 	"debug": zap.DebugLevel,
// 	"info":  zap.InfoLevel,
// 	"error": zap.ErrorLevel,
// }

// TODO Add level to disable stacktraces.
// https://github.com/kubernetes-sigs/controller-runtime/issues/1035
var stackLevelStrings = map[string]zapcore.Level{
	"info":  zap.InfoLevel,
	"error": zap.ErrorLevel,
}

//
// type encoderFlag struct {
// 	setFunc func(NewEncoderFunc)
// 	value   string
// }

// var _ flag.Value = &encoderFlag{}
//
// func (ev *encoderFlag) String() string {
// 	return ev.value
// }
//
// func (ev *encoderFlag) Type() string {
// 	return "encoder"
// }

//
// func (ev *encoderFlag) Set(flagValue string) error {
// 	val := strings.ToLower(flagValue)
// 	switch val {
// 	case "json":
// 		ev.setFunc(newJSONEncoder)
// 	case "console":
// 		ev.setFunc(newConsoleEncoder)
// 	default:
// 		return fmt.Errorf("invalid encoder value \"%s\"", flagValue)
// 	}
// 	ev.value = flagValue
// 	return nil
// }

// type levelFlag struct {
// 	setFunc func(zapcore.LevelEnabler)
// 	value   string
// }

// var _ flag.Value = &levelFlag{}

// func (ev *levelFlag) Set(flagValue string) error {
// 	level, validLevel := levelStrings[strings.ToLower(flagValue)]
// 	if !validLevel {
// 		logLevel, err := strconv.Atoi(flagValue)
// 		if err != nil {
// 			return fmt.Errorf("invalid log level \"%s\"", flagValue)
// 		}
// 		if logLevel > 0 {
// 			intLevel := -1 * logLevel
// 			ev.setFunc(zap.NewAtomicLevelAt(zapcore.Level(int8(intLevel))))
// 		} else {
// 			return fmt.Errorf("invalid log level \"%s\"", flagValue)
// 		}
// 	} else {
// 		ev.setFunc(zap.NewAtomicLevelAt(level))
// 	}
// 	ev.value = flagValue
// 	return nil
// }
//
// func (ev *levelFlag) String() string {
// 	return ev.value
// }
//
// func (ev *levelFlag) Type() string {
// 	return "level"
// }

type stackTraceFlag struct {
	setFunc func(zapcore.LevelEnabler)
	value   string
}

var _ flag.Value = &stackTraceFlag{}

func (ev *stackTraceFlag) Set(flagValue string) error {
	level, validLevel := stackLevelStrings[strings.ToLower(flagValue)]
	if !validLevel {
		return fmt.Errorf("invalid stacktrace level \"%s\"", flagValue)
	}

	ev.setFunc(zap.NewAtomicLevelAt(level))
	ev.value = flagValue

	return nil
}

func (ev *stackTraceFlag) String() string {
	return ev.value
}

func (ev *stackTraceFlag) Type() string {
	return "level"
}

// // EncoderConfigOption is a function that can modify a `zapcore.EncoderConfig`.
// type EncoderConfigOption func(*zapcore.EncoderConfig)

// // NewEncoderFunc is a function that creates an Encoder using the provided EncoderConfigOptions.
// type NewEncoderFunc func(...EncoderConfigOption) zapcore.Encoder

// // UseDevMode sets the logger to use (or not use) development mode (more
// // human-readable output, extra stack traces and logging information, etc).
// // See Options.Development
// func UseDevMode(enabled bool) Opts {
// 	return func(o *Options) {
// 		o.Development = enabled
// 	}
// }

// WriteTo configures the logger to write to the given io.Writer, instead of standard error.
// See Options.DestWritter
// func WriteTo(out io.Writer) Opts {
// 	return func(o *Options) {
// 		o.DestWritter = out
// 	}
// }

// // Encoder configures how the logger will encode the output e.g JSON or console.
// // See Options.Encoder
// func Encoder(encoder zapcore.Encoder) func(o *Options) {
// 	return func(o *Options) {
// 		o.Encoder = encoder
// 	}
// }
//
// // JSONEncoder configures the logger to use a JSON Encoder
// func JSONEncoder(opts ...EncoderConfigOption) func(o *Options) {
// 	return func(o *Options) {
// 		o.Encoder = newJSONEncoder(opts...)
// 	}
// }
//
// func newJSONEncoder(opts ...EncoderConfigOption) zapcore.Encoder {
// 	encoderConfig := zap.NewProductionEncoderConfig()
// 	for _, opt := range opts {
// 		opt(&encoderConfig)
// 	}
// 	return zapcore.NewJSONEncoder(encoderConfig)
// }
//
// // ConsoleEncoder configures the logger to use a Console encoder
// func ConsoleEncoder(opts ...EncoderConfigOption) func(o *Options) {
// 	return func(o *Options) {
// 		o.Encoder = newConsoleEncoder(opts...)
// 	}
// }
//
// func newConsoleEncoder(opts ...EncoderConfigOption) zapcore.Encoder {
// 	encoderConfig := zap.NewDevelopmentEncoderConfig()
// 	for _, opt := range opts {
// 		opt(&encoderConfig)
// 	}
// 	return zapcore.NewConsoleEncoder(encoderConfig)
// }
//
// // Level sets the the minimum enabled logging level e.g Debug, Info
// // See Options.Level
// func Level(level zapcore.LevelEnabler) func(o *Options) {
// 	return func(o *Options) {
// 		o.Level = level
// 	}
// }
//
// // StacktraceLevel configures the logger to record a stack trace for all messages at
// // or above a given level.
// // See Options.StacktraceLevel
// func StacktraceLevel(stacktraceLevel zapcore.LevelEnabler) func(o *Options) {
// 	return func(o *Options) {
// 		o.StacktraceLevel = stacktraceLevel
// 	}
// }
//
// // RawZapOpts allows appending arbitrary zap.Options to configure the underlying zap logger.
// // See Options.ZapOpts
// func RawZapOpts(zapOpts ...zap.Option) func(o *Options) {
// 	return func(o *Options) {
// 		o.ZapOpts = append(o.ZapOpts, zapOpts...)
// 	}
// }
//
// // addDefaults adds defaults to the Options
// func (o *Options) addDefaults() {
// 	if o.DestWritter == nil {
// 		o.DestWritter = os.Stderr
// 	}
//
// 	if o.Development {
// 		if o.NewEncoder == nil {
// 			o.NewEncoder = newConsoleEncoder
// 		}
// 		if o.Level == nil {
// 			lvl := zap.NewAtomicLevelAt(zap.DebugLevel)
// 			o.Level = &lvl
// 		}
// 		if o.StacktraceLevel == nil {
// 			lvl := zap.NewAtomicLevelAt(zap.WarnLevel)
// 			o.StacktraceLevel = &lvl
// 		}
// 		o.ZapOpts = append(o.ZapOpts, zap.Development())
//
// 	} else {
// 		if o.NewEncoder == nil {
// 			o.NewEncoder = newJSONEncoder
// 		}
// 		if o.Level == nil {
// 			lvl := zap.NewAtomicLevelAt(zap.InfoLevel)
// 			o.Level = &lvl
// 		}
// 		if o.StacktraceLevel == nil {
// 			lvl := zap.NewAtomicLevelAt(zap.ErrorLevel)
// 			o.StacktraceLevel = &lvl
// 		}
// 		// Disable sampling for increased Debug levels. Otherwise, this will
// 		// cause index out of bounds errors in the sampling code.
// 		if !o.Level.Enabled(zapcore.Level(-2)) {
// 			o.ZapOpts = append(o.ZapOpts,
// 				zap.WrapCore(func(core zapcore.Core) zapcore.Core {
// 					return zapcore.NewSampler(core, time.Second, 100, 100)
// 				}))
// 		}
// 	}
// 	if o.Encoder == nil {
// 		o.Encoder = o.NewEncoder(o.EncoderConfigOptions...)
// 	}
// 	o.ZapOpts = append(o.ZapOpts, zap.AddStacktrace(o.StacktraceLevel))
// }
//
// // UseFlagOptions configures the logger to use the Options set by parsing zap option flags from the CLI.
// //  opts := zap.Options{}
// //  opts.BindFlags(flag.CommandLine)
// //  flag.Parse()
// //  log := zap.New(zap.UseFlagOptions(&opts))
// func UseFlagOptions(in *Options) Opts {
// 	return func(o *Options) {
// 		*o = *in
// 	}
// }
