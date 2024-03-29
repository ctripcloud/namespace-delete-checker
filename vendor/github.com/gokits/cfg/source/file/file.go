package file

import (
	"context"
	"io/ioutil"
	"os"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	logger "github.com/gokits/stdlogger"
	"github.com/gokits/stdlogger/nooplogger"
)

type File struct {
	filename string
	watcher  *fsnotify.Watcher
	rw       sync.RWMutex
	logger   logger.LeveledLogger
	content  []byte
	ver      int64
	c        chan struct{}
}

type Option func(*File)

func WithLogger(log logger.LeveledLogger) Option {
	return func(f *File) {
		f.logger = log
	}
}

func NewFileSource(filename string, opts ...Option) (fs *File, err error) {
	fs = &File{
		filename: filename,
		logger:   nooplogger.Default(),
	}
	for _, opt := range opts {
		opt(fs)
	}
	if fs.watcher, err = fsnotify.NewWatcher(); err != nil {
		return
	}
	defer func() {
		if err != nil {
			fs.watcher.Close()
		}
	}()
	if err = fs.watcher.Add(filename); err != nil {
		return
	}
	fs.c = make(chan struct{})
	go fs.run()
	return
}

func (f *File) readfile() error {
	fh, err := os.OpenFile(f.filename, os.O_RDONLY, 0444)
	if err != nil {
		return err
	}
	defer fh.Close()
	ct, err := ioutil.ReadAll(fh)
	if err != nil {
		return err
	}

	f.rw.Lock()
	close(f.c)
	f.c = make(chan struct{})
	f.content = ct
	f.ver += 1
	f.rw.Unlock()
	return nil
}

func (f *File) run() {
	var err error
	if err = f.readfile(); err != nil {
		f.logger.Warnf("readfile of %s failed: %v", f.filename, err)
	}
	watched := true
	for {
		for !watched {
			select {
			case e, ok := <-f.watcher.Events:
				if !ok {
					f.Close()
					return
				}
				f.logger.Warnf("unexpected watched event %v", e)
			case _, ok := <-f.watcher.Errors:
				f.Close()
				if ok {
					f.logger.Warnf("watched error %v", err)
				}
				return
			default:
			}
			if err = f.watcher.Add(f.filename); err != nil {
				f.logger.Errorf("watcher read %s failed: %v", f.filename, err)
				time.Sleep(time.Second)
				continue
			}
			watched = true
			if err = f.readfile(); err != nil {
				f.logger.Warnf("readfile of %s failed: %v", f.filename, err)
			}
		}

		select {
		case ev, ok := <-f.watcher.Events:
			if !ok {
				f.Close()
				return
			}
			if ev.Op&(fsnotify.Create|fsnotify.Rename|fsnotify.Write) != 0 {
				if err = f.readfile(); err != nil {
					f.logger.Warnf("readfile of %s failed: %v", f.filename, err)
					continue
				}
			}
			if ev.Op&fsnotify.Remove != 0 {
				f.logger.Infof("watched remove event of %s", f.filename)
				if err = f.watcher.Remove(f.filename); err != nil {
					f.logger.Errorf("watcher remove %s failed: %v", f.filename, err)
				}
				watched = false
			}
		case _, ok := <-f.watcher.Errors:
			f.Close()
			if ok {
				f.logger.Warnf("readfile of %s failed: %v", f.filename, err)
			}
			return
		}
	}
}

func (f *File) Next(ctx context.Context, preversion int64) (content []byte, curversion int64, ok bool) {
	f.rw.RLock()
	if f.ver != preversion {
		defer f.rw.RUnlock()
		content, curversion, ok = f.content, f.ver, true
		return
	}
	copyc := f.c
	f.rw.RUnlock()
	select {
	case <-ctx.Done():
		return
	case <-copyc:
		f.rw.RLock()
		content, curversion, ok = f.content, f.ver, true
		f.rw.RUnlock()
		return
	}
}

func (f *File) Close() {
	f.watcher.Close()
	f.rw.Lock()
	defer f.rw.Unlock()
	select {
	case <-f.c:
		return
	default:
		close(f.c)
	}
}
