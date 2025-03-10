package stats

import (
	"github.com/TeaOSLab/EdgeCommon/pkg/nodeconfigs"
	"github.com/TeaOSLab/EdgeCommon/pkg/rpc/pb"
	"github.com/TeaOSLab/EdgeNode/internal/events"
	"github.com/TeaOSLab/EdgeNode/internal/goman"
	"github.com/TeaOSLab/EdgeNode/internal/monitor"
	"github.com/TeaOSLab/EdgeNode/internal/remotelogs"
	"github.com/TeaOSLab/EdgeNode/internal/rpc"
	"github.com/TeaOSLab/EdgeNode/internal/utils"
	"github.com/iwind/TeaGo/Tea"
	"github.com/iwind/TeaGo/maps"
	"github.com/iwind/TeaGo/types"
	"strconv"
	"strings"
	"sync"
	"time"
)

var SharedTrafficStatManager = NewTrafficStatManager()

type TrafficItem struct {
	Bytes                int64
	CachedBytes          int64
	CountRequests        int64
	CountCachedRequests  int64
	CountAttackRequests  int64
	AttackBytes          int64
	PlanId               int64
	CheckingTrafficLimit bool
}

func (this *TrafficItem) Add(anotherItem *TrafficItem) {
	this.Bytes += anotherItem.Bytes
	this.CachedBytes += anotherItem.CachedBytes
	this.CountRequests += anotherItem.CountRequests
	this.CountCachedRequests += anotherItem.CountCachedRequests
	this.CountAttackRequests += anotherItem.CountAttackRequests
	this.AttackBytes += anotherItem.AttackBytes
}

const trafficStatsMaxLife = 1200 // 最大只保存20分钟内的数据

// TrafficStatManager 区域流量统计
type TrafficStatManager struct {
	itemMap    map[string]*TrafficItem // [timestamp serverId] => *TrafficItem
	domainsMap map[string]*TrafficItem // timestamp @ serverId @ domain => *TrafficItem

	pbItems       []*pb.ServerDailyStat
	pbDomainItems []*pb.UploadServerDailyStatsRequest_DomainStat

	locker sync.Mutex

	totalRequests int64
}

// NewTrafficStatManager 获取新对象
func NewTrafficStatManager() *TrafficStatManager {
	var manager = &TrafficStatManager{
		itemMap:    map[string]*TrafficItem{},
		domainsMap: map[string]*TrafficItem{},
	}

	return manager
}

// Start 启动自动任务
func (this *TrafficStatManager) Start() {
	// 上传请求总数
	var monitorTicker = time.NewTicker(1 * time.Minute)
	events.OnKey(events.EventQuit, this, func() {
		monitorTicker.Stop()
	})
	goman.New(func() {
		for range monitorTicker.C {
			if this.totalRequests > 0 {
				monitor.SharedValueQueue.Add(nodeconfigs.NodeValueItemRequests, maps.Map{"total": this.totalRequests})
				this.totalRequests = 0
			}
		}
	})

	// 上传统计数据
	var duration = 5 * time.Minute
	if Tea.IsTesting() {
		// 测试环境缩短上传时间，方便我们调试
		duration = 30 * time.Second
	}
	var ticker = time.NewTicker(duration)
	events.OnKey(events.EventQuit, this, func() {
		remotelogs.Println("TRAFFIC_STAT_MANAGER", "quit")
		ticker.Stop()
	})
	remotelogs.Println("TRAFFIC_STAT_MANAGER", "start ...")
	for range ticker.C {
		err := this.Upload()
		if err != nil {
			if !rpc.IsConnError(err) {
				remotelogs.Error("TRAFFIC_STAT_MANAGER", "upload stats failed: "+err.Error())
			} else {
				remotelogs.Warn("TRAFFIC_STAT_MANAGER", "upload stats failed: "+err.Error())
			}
		}
	}
}

// Add 添加流量
func (this *TrafficStatManager) Add(serverId int64, domain string, bytes int64, cachedBytes int64, countRequests int64, countCachedRequests int64, countAttacks int64, attackBytes int64, checkingTrafficLimit bool, planId int64) {
	if serverId == 0 {
		return
	}

	if bytes == 0 && countRequests == 0 {
		return
	}

	this.totalRequests++

	var timestamp = utils.FloorUnixTime(300)
	var key = strconv.FormatInt(timestamp, 10) + strconv.FormatInt(serverId, 10)
	this.locker.Lock()

	// 总的流量
	item, ok := this.itemMap[key]
	if !ok {
		item = &TrafficItem{}
		this.itemMap[key] = item
	}
	item.Bytes += bytes
	item.CachedBytes += cachedBytes
	item.CountRequests += countRequests
	item.CountCachedRequests += countCachedRequests
	item.CountAttackRequests += countAttacks
	item.AttackBytes += attackBytes
	item.CheckingTrafficLimit = checkingTrafficLimit
	item.PlanId = planId

	// 单个域名流量
	var domainKey = strconv.FormatInt(timestamp, 10) + "@" + strconv.FormatInt(serverId, 10) + "@" + domain
	domainItem, ok := this.domainsMap[domainKey]
	if !ok {
		domainItem = &TrafficItem{}
		this.domainsMap[domainKey] = domainItem
	}
	domainItem.Bytes += bytes
	domainItem.CachedBytes += cachedBytes
	domainItem.CountRequests += countRequests
	domainItem.CountCachedRequests += countCachedRequests
	domainItem.CountAttackRequests += countAttacks
	domainItem.AttackBytes += attackBytes

	this.locker.Unlock()
}

// Upload 上传流量
func (this *TrafficStatManager) Upload() error {
	var regionId int64
	nodeConfig, _ := nodeconfigs.SharedNodeConfig()
	if nodeConfig != nil {
		regionId = nodeConfig.RegionId
	}

	client, err := rpc.SharedRPC()
	if err != nil {
		return err
	}

	this.locker.Lock()

	var itemMap = this.itemMap
	var domainMap = this.domainsMap

	// reset
	this.itemMap = map[string]*TrafficItem{}
	this.domainsMap = map[string]*TrafficItem{}

	this.locker.Unlock()

	// 服务统计
	var pbServerStats = []*pb.ServerDailyStat{}
	for key, item := range itemMap {
		timestamp, err := strconv.ParseInt(key[:10], 10, 64)
		if err != nil {
			return err
		}
		serverId, err := strconv.ParseInt(key[10:], 10, 64)
		if err != nil {
			return err
		}

		pbServerStats = append(pbServerStats, &pb.ServerDailyStat{
			ServerId:             serverId,
			NodeRegionId:         regionId,
			Bytes:                item.Bytes,
			CachedBytes:          item.CachedBytes,
			CountRequests:        item.CountRequests,
			CountCachedRequests:  item.CountCachedRequests,
			CountAttackRequests:  item.CountAttackRequests,
			AttackBytes:          item.AttackBytes,
			CheckTrafficLimiting: item.CheckingTrafficLimit,
			PlanId:               item.PlanId,
			CreatedAt:            timestamp,
		})
	}

	// 域名统计
	var pbDomainStats = []*pb.UploadServerDailyStatsRequest_DomainStat{}
	for key, item := range domainMap {
		var pieces = strings.SplitN(key, "@", 3)
		if len(pieces) != 3 {
			continue
		}
		pbDomainStats = append(pbDomainStats, &pb.UploadServerDailyStatsRequest_DomainStat{
			ServerId:            types.Int64(pieces[1]),
			Domain:              pieces[2],
			Bytes:               item.Bytes,
			CachedBytes:         item.CachedBytes,
			CountRequests:       item.CountRequests,
			CountCachedRequests: item.CountCachedRequests,
			CountAttackRequests: item.CountAttackRequests,
			AttackBytes:         item.AttackBytes,
			CreatedAt:           types.Int64(pieces[0]),
		})
	}

	// 历史未提交记录
	if len(this.pbItems) > 0 || len(this.pbDomainItems) > 0 {
		var expiredAt = time.Now().Unix() - 1200 // 只保留20分钟

		for _, item := range this.pbItems {
			if item.CreatedAt > expiredAt {
				pbServerStats = append(pbServerStats, item)
			}
		}
		this.pbItems = nil

		for _, item := range this.pbDomainItems {
			if item.CreatedAt > expiredAt {
				pbDomainStats = append(pbDomainStats, item)
			}
		}
		this.pbDomainItems = nil
	}

	if len(pbServerStats) == 0 && len(pbDomainStats) == 0 {
		return nil
	}

	_, err = client.ServerDailyStatRPC.UploadServerDailyStats(client.Context(), &pb.UploadServerDailyStatsRequest{
		Stats:       pbServerStats,
		DomainStats: pbDomainStats,
	})
	if err != nil {
		// 加回历史记录
		this.pbItems = pbServerStats
		this.pbDomainItems = pbDomainStats

		return err
	}

	return nil
}
