// Copyright 2021 Liuxiangchao iwind.liu@gmail.com. All rights reserved.

package caches

import (
	"database/sql"
	"github.com/TeaOSLab/EdgeNode/internal/goman"
	"github.com/TeaOSLab/EdgeNode/internal/remotelogs"
	"github.com/TeaOSLab/EdgeNode/internal/ttlcache"
	"github.com/TeaOSLab/EdgeNode/internal/utils/fnv"
	"github.com/iwind/TeaGo/types"
	_ "github.com/mattn/go-sqlite3"
	"os"
	"sync/atomic"
	"time"
)

const CountFileDB = 20

// FileList 文件缓存列表管理
type FileList struct {
	dir    string
	dbList [CountFileDB]*FileListDB
	total  int64

	onAdd    func(item *Item)
	onRemove func(item *Item)

	memoryCache *ttlcache.Cache

	// 老数据库地址
	oldDir string
}

func NewFileList(dir string) ListInterface {
	return &FileList{
		dir:         dir,
		memoryCache: ttlcache.NewCache(),
	}
}

func (this *FileList) SetOldDir(oldDir string) {
	this.oldDir = oldDir
}

func (this *FileList) Init() error {
	// 检查目录是否存在
	_, err := os.Stat(this.dir)
	if err != nil {
		err = os.MkdirAll(this.dir, 0777)
		if err != nil {
			return err
		}
		remotelogs.Println("CACHE", "create cache dir '"+this.dir+"'")
	}

	var dir = this.dir
	if dir == "/" {
		// 防止sqlite提示authority错误
		dir = ""
	}

	remotelogs.Println("CACHE", "loading database from '"+dir+"' ...")
	for i := 0; i < CountFileDB; i++ {
		var db = NewFileListDB()
		err = db.Open(dir + "/db-" + types.String(i) + ".db")
		if err != nil {
			return err
		}

		err = db.Init()
		if err != nil {
			return err
		}

		this.dbList[i] = db
	}

	// 读取总数量
	this.total = 0
	for _, db := range this.dbList {
		this.total += db.total
	}

	// 升级老版本数据库
	goman.New(func() {
		this.upgradeOldDB()
	})

	return nil
}

func (this *FileList) Reset() error {
	// 不做任何事情
	return nil
}

func (this *FileList) Add(hash string, item *Item) error {
	var db = this.GetDB(hash)

	if !db.IsReady() {
		return nil
	}

	err := db.AddAsync(hash, item)
	if err != nil {
		return err
	}

	atomic.AddInt64(&this.total, 1)

	// 这里不增加点击量，以减少对数据库的操作次数

	this.memoryCache.Write(hash, 1, item.ExpiredAt)

	if this.onAdd != nil {
		this.onAdd(item)
	}
	return nil
}

func (this *FileList) Exist(hash string) (bool, error) {
	var db = this.GetDB(hash)

	if !db.IsReady() {
		return false, nil
	}

	// 如果Hash列表里不存在，那么必然不存在
	if !db.hashMap.Exist(hash) {
		return false, nil
	}

	var item = this.memoryCache.Read(hash)
	if item != nil {
		return true, nil
	}

	var row = db.existsByHashStmt.QueryRow(hash, time.Now().Unix())
	if row.Err() != nil {
		return false, nil
	}

	var expiredAt int64
	err := row.Scan(&expiredAt)
	if err != nil {
		if err == sql.ErrNoRows {
			err = nil
		}
		return false, err
	}
	this.memoryCache.Write(hash, 1, expiredAt)
	return true, nil
}

// CleanPrefix 清理某个前缀的缓存数据
func (this *FileList) CleanPrefix(prefix string) error {
	if len(prefix) == 0 {
		return nil
	}

	defer func() {
		// TODO 需要优化
		this.memoryCache.Clean()
	}()

	for _, db := range this.dbList {
		err := db.CleanPrefix(prefix)
		if err != nil {
			return err
		}
	}
	return nil
}

// CleanMatchKey 清理通配符匹配的缓存数据，类似于 https://*.example.com/hello
func (this *FileList) CleanMatchKey(key string) error {
	if len(key) == 0 {
		return nil
	}

	defer func() {
		// TODO 需要优化
		this.memoryCache.Clean()
	}()

	for _, db := range this.dbList {
		err := db.CleanMatchKey(key)
		if err != nil {
			return err
		}
	}
	return nil
}

// CleanMatchPrefix 清理通配符匹配的缓存数据，类似于 https://*.example.com/prefix/
func (this *FileList) CleanMatchPrefix(prefix string) error {
	if len(prefix) == 0 {
		return nil
	}

	defer func() {
		// TODO 需要优化
		this.memoryCache.Clean()
	}()

	for _, db := range this.dbList {
		err := db.CleanMatchPrefix(prefix)
		if err != nil {
			return err
		}
	}
	return nil
}

func (this *FileList) Remove(hash string) error {
	_, err := this.remove(hash)
	return err
}

// Purge 清理过期的缓存
// count 每次遍历的最大数量，控制此数字可以保证每次清理的时候不用花太多时间
// callback 每次发现过期key的调用
func (this *FileList) Purge(count int, callback func(hash string) error) (int, error) {
	count /= CountFileDB
	if count <= 0 {
		count = 100
	}

	var countFound = 0
	for _, db := range this.dbList {
		hashStrings, err := db.ListExpiredItems(count)
		if err != nil {
			return 0, nil
		}
		countFound += len(hashStrings)

		// 不在 rows.Next() 循环中操作是为了避免死锁
		for _, hash := range hashStrings {
			err = this.Remove(hash)
			if err != nil {
				return 0, err
			}

			err = callback(hash)
			if err != nil {
				return 0, err
			}
		}
	}

	return countFound, nil
}

func (this *FileList) PurgeLFU(count int, callback func(hash string) error) error {
	count /= CountFileDB
	if count <= 0 {
		count = 100
	}

	for _, db := range this.dbList {
		hashStrings, err := db.ListLFUItems(count)
		if err != nil {
			return err
		}

		// 不在 rows.Next() 循环中操作是为了避免死锁
		for _, hash := range hashStrings {
			notFound, err := this.remove(hash)
			if err != nil {
				return err
			}
			if notFound {
				err = db.DeleteHitAsync(hash)
				if err != nil {
					return db.WrapError(err)
				}
			}

			err = callback(hash)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func (this *FileList) CleanAll() error {
	defer this.memoryCache.Clean()

	for _, db := range this.dbList {
		err := db.CleanAll()
		if err != nil {
			return err
		}
	}

	atomic.StoreInt64(&this.total, 0)

	return nil
}

func (this *FileList) Stat(check func(hash string) bool) (*Stat, error) {
	var result = &Stat{}

	for _, db := range this.dbList {
		if !db.IsReady() {
			return &Stat{}, nil
		}

		// 这里不设置过期时间、不使用 check 函数，目的是让查询更快速一些
		_ = check

		var row = db.statStmt.QueryRow()
		if row.Err() != nil {
			return nil, row.Err()
		}
		var stat = &Stat{}
		err := row.Scan(&stat.Count, &stat.Size, &stat.ValueSize)
		if err != nil {
			return nil, err
		}
		result.Count += stat.Count
		result.Size += stat.Size
		result.ValueSize += stat.ValueSize
	}

	return result, nil
}

// Count 总数量
// 常用的方法，所以避免直接查询数据库
func (this *FileList) Count() (int64, error) {
	return atomic.LoadInt64(&this.total), nil
}

// IncreaseHit 增加点击量
func (this *FileList) IncreaseHit(hash string) error {
	var db = this.GetDB(hash)

	if !db.IsReady() {
		return nil
	}

	return db.IncreaseHitAsync(hash)
}

// OnAdd 添加事件
func (this *FileList) OnAdd(f func(item *Item)) {
	this.onAdd = f
}

// OnRemove 删除事件
func (this *FileList) OnRemove(f func(item *Item)) {
	this.onRemove = f
}

func (this *FileList) Close() error {
	this.memoryCache.Destroy()

	for _, db := range this.dbList {
		if db != nil {
			_ = db.Close()
		}
	}

	return nil
}

func (this *FileList) GetDBIndex(hash string) uint64 {
	return fnv.HashString(hash) % CountFileDB
}

func (this *FileList) GetDB(hash string) *FileListDB {
	return this.dbList[fnv.HashString(hash)%CountFileDB]
}

func (this *FileList) remove(hash string) (notFound bool, err error) {
	var db = this.GetDB(hash)

	if !db.IsReady() {
		return false, nil
	}

	// HashMap中不存在，则确定不存在
	if !db.hashMap.Exist(hash) {
		return true, nil
	}
	defer db.hashMap.Delete(hash)

	// 从缓存中删除
	this.memoryCache.Delete(hash)

	var row = db.selectByHashStmt.QueryRow(hash)
	if row.Err() != nil {
		if row.Err() == sql.ErrNoRows {
			return true, nil
		}
		return false, row.Err()
	}

	var item = &Item{Type: ItemTypeFile}
	err = row.Scan(&item.Key, &item.HeaderSize, &item.BodySize, &item.MetaSize, &item.ExpiredAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return true, nil
		}
		return false, err
	}

	err = db.DeleteAsync(hash)
	if err != nil {
		return false, db.WrapError(err)
	}

	atomic.AddInt64(&this.total, -1)

	err = db.DeleteHitAsync(hash)
	if err != nil {
		return false, db.WrapError(err)
	}

	if this.onRemove != nil {
		this.onRemove(item)
	}

	return false, nil
}

// 升级老版本数据库
func (this *FileList) upgradeOldDB() {
	if len(this.oldDir) == 0 {
		return
	}
	_ = this.UpgradeV3(this.oldDir, false)
}

func (this *FileList) UpgradeV3(oldDir string, brokenOnError bool) error {
	// index.db
	var indexDBPath = oldDir + "/index.db"
	_, err := os.Stat(indexDBPath)
	if err != nil {
		return nil
	}
	remotelogs.Println("CACHE", "upgrading local database from '"+oldDir+"' ...")

	defer func() {
		_ = os.Remove(indexDBPath)
		remotelogs.Println("CACHE", "upgrading local database finished")
	}()

	db, err := sql.Open("sqlite3", "file:"+indexDBPath+"?cache=shared&mode=rwc&_journal_mode=WAL&_sync=OFF")
	if err != nil {
		return err
	}

	defer func() {
		_ = db.Close()
	}()

	var isFinished = false
	var offset = 0
	var count = 10000

	for {
		if isFinished {
			break
		}

		err = func() error {
			defer func() {
				offset += count
			}()

			rows, err := db.Query(`SELECT "hash", "key", "headerSize", "bodySize", "metaSize", "expiredAt", "staleAt", "createdAt", "host", "serverId" FROM "cacheItems_v3" ORDER BY "id" ASC LIMIT ?, ?`, offset, count)
			if err != nil {
				return err
			}
			defer func() {
				_ = rows.Close()
			}()

			var hash = ""
			var key = ""
			var headerSize int64
			var bodySize int64
			var metaSize int64
			var expiredAt int64
			var staleAt int64
			var createdAt int64
			var host string
			var serverId int64

			isFinished = true

			for rows.Next() {
				isFinished = false

				err = rows.Scan(&hash, &key, &headerSize, &bodySize, &metaSize, &expiredAt, &staleAt, &createdAt, &host, &serverId)
				if err != nil {
					if brokenOnError {
						return err
					}
					return nil
				}

				err = this.Add(hash, &Item{
					Type:       ItemTypeFile,
					Key:        key,
					ExpiredAt:  expiredAt,
					StaleAt:    staleAt,
					HeaderSize: headerSize,
					BodySize:   bodySize,
					MetaSize:   metaSize,
					Host:       host,
					ServerId:   serverId,
					Week1Hits:  0,
					Week2Hits:  0,
					Week:       0,
				})
				if err != nil {
					if brokenOnError {
						return err
					}
				}
			}

			return nil
		}()
		if err != nil {
			return err
		}

		time.Sleep(1 * time.Second)
	}

	return nil
}
