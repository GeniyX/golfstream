package backend

import (
	"fmt"
	"github.com/Monnoroch/golfstream/errors"
	"github.com/Monnoroch/golfstream/stream"
	"github.com/siddontang/ledisdb/config"
	"github.com/siddontang/ledisdb/ledis"
	"log"
	"math"
	"os"
	"sync"
)

type ledisListStream struct {
	db      *ledis.DB
	key     []byte
	num     int32
	l       int32
	delLock *sync.RWMutex
}

func (self *ledisListStream) Next() (stream.Event, error) {
	if self.num >= self.l {
		self.delLock.RUnlock()
		return nil, stream.EOI
	}

	res, err := self.db.LIndex(self.key, self.num)
	if err != nil {
		self.delLock.RUnlock()
		return nil, err
	}

	self.num += 1
	return stream.Event(res), nil
}

type ledisStreamObj struct {
	db   *ledis.DB
	back *ledisBackend
	name string
	key  []byte

	delLock sync.RWMutex

	refcnt int
}

func (self *ledisStreamObj) Add(evt stream.Event) error {
	bs, ok := evt.([]byte)
	if !ok {
		return errors.New(fmt.Sprintf("ledisStreamObj.Add: Expected []byte, got %v", evt))
	}

	self.delLock.RLock()
	defer self.delLock.RUnlock()

	_, err := self.db.RPush(self.key, bs)
	return err
}

func (self *ledisStreamObj) Read(afrom uint, to int) (stream.Stream, error) {
	from := int(afrom)
	if from == to {
		return stream.Empty(), nil
	}

	self.delLock.RLock()

	al, err := self.db.LLen(self.key)
	if err != nil {
		return nil, err
	}
	l := int(al)

	if to < 0 {
		to = l + 1 + to
	}
	if from < 0 {
		from = l + 1 + from
	}

	if int(from) == to {
		return stream.Empty(), nil
	}

	if err := checkRange(int(from), int(to), int(l), "ledisStreamObj.Read"); err != nil {
		return nil, err
	}

	return &ledisListStream{self.db, self.key, int32(from), int32(to), &self.delLock}, nil
}

func (self *ledisStreamObj) Del(afrom uint, ato int) (bool, error) {
	from := int64(afrom)
	to := int64(ato)
	if from == to {
		return true, nil
	}

	self.delLock.Lock()
	defer self.delLock.Unlock()

	if from == 0 && to == -1 {
		cnt, err := self.db.LClear(self.key)
		if err != nil {
			return false, err
		}
		return cnt != 0, nil
	}

	l, err := self.db.LLen(self.key)
	if err != nil {
		return false, err
	}

	if to < 0 {
		to = l + 1 + to
	}
	if from < 0 {
		from = l + 1 + from
	}

	if from == 0 && to == l {
		cnt, err := self.db.LClear(self.key)
		if err != nil {
			return false, err
		}
		return cnt != 0, nil
	}

	if from == 0 {
		err := self.db.LTrim(self.key, int64(to-1), l)
		return err == nil, err
	}

	if to == l {
		err := self.db.LTrim(self.key, 0, from-1)
		return err == nil, err
	}

	if err := checkRange(int(from), int(to), int(l), "ledisStreamObj.Del"); err != nil {
		return false, err
	}

	// TODO: optimize: read smaller part to the memory
	rest, err := self.db.LRange(self.key, int32(to), int32(l))
	if err != nil {
		return false, err
	}

	if err := self.db.LTrim(self.key, 0, from-1); err != nil {
		return false, err
	}

	// TODO: if this fails, we should roll back the trim... but whatever. For now.
	_, err = self.db.RPush(self.key, rest...)
	if err != nil {
		log.Println(fmt.Sprintf("ledisStreamObj.Del: WARNING: RPush failed, but Trim wasn't rolled back. Lost the data."))
	}
	return err == nil, err
}

func (self *ledisStreamObj) Len() (uint, error) {
	l, err := self.db.LLen(self.key)
	if err != nil {
		return 0, err
	}

	return uint(l), nil
}

func (self *ledisStreamObj) Close() error {
	return nil
}

type ledisBackend struct {
	dirname string
	ledis   *ledis.Ledis
	db      *ledis.DB
	lock    sync.Mutex
	data    map[string]*ledisStreamObj
}

func (self *ledisBackend) Config() (interface{}, error) {
	return map[string]interface{}{
		"type": "ledis",
		"arg":  self.dirname,
	}, nil
}

func (self *ledisBackend) Streams() ([]string, error) {
	keys, err := self.db.Scan(ledis.KV, []byte{}, int(math.MaxInt32), true, "")
	if err != nil {
		return nil, err
	}

	res := make([]string, len(keys))
	for i, v := range keys {
		res[i] = string(v)
	}
	return res, nil
}

func (self *ledisBackend) GetStream(name string) (BackendStream, error) {
	self.lock.Lock()
	defer self.lock.Unlock()

	v, ok := self.data[name]
	if !ok {
		v = &ledisStreamObj{self.db, self, name, []byte(name), sync.RWMutex{}, 0}
		self.data[name] = v
	}

	v.refcnt += 1
	return v, nil
}

func (self *ledisBackend) Drop() error {
	return errors.List().
		Add(self.ledis.FlushAll()).
		Add(os.RemoveAll(self.dirname)).
		Err()
}

func (self *ledisBackend) Close() error {
	self.data = nil
	self.ledis.Close()
	return nil
}

func (self *ledisBackend) release(s *ledisStreamObj) {
	self.lock.Lock()
	defer self.lock.Unlock()

	s.refcnt -= 1
	if s.refcnt == 0 {
		delete(self.data, s.name)
	}
}

func NewLedis(dirname string) (Backend, error) {
	lcfg := config.NewConfigDefault()
	lcfg.DataDir = dirname
	lcfg.Addr = ""
	lcfg.Databases = 1

	ledis, err := ledis.Open(lcfg)
	if err != nil {
		return nil, err
	}

	db, err := ledis.Select(0)
	if err != nil {
		return nil, err
	}

	return &ledisBackend{dirname, ledis, db, sync.Mutex{}, map[string]*ledisStreamObj{}}, nil
}