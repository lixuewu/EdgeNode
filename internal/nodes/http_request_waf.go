package nodes

import (
	"bytes"
	iplib "github.com/TeaOSLab/EdgeCommon/pkg/iplibrary"
	"github.com/TeaOSLab/EdgeCommon/pkg/serverconfigs/firewallconfigs"
	"github.com/TeaOSLab/EdgeNode/internal/iplibrary"
	"github.com/TeaOSLab/EdgeNode/internal/remotelogs"
	"github.com/TeaOSLab/EdgeNode/internal/stats"
	"github.com/TeaOSLab/EdgeNode/internal/waf"
	"github.com/iwind/TeaGo/Tea"
	"github.com/iwind/TeaGo/lists"
	"github.com/iwind/TeaGo/types"
	"io"
	"net/http"
)

// 调用WAF
func (this *HTTPRequest) doWAFRequest() (blocked bool) {
	if this.web.FirewallRef == nil || !this.web.FirewallRef.IsOn {
		return
	}

	var remoteAddr = this.requestRemoteAddr(true)

	// 检查是否为白名单直连
	if !Tea.IsTesting() && this.nodeConfig.IPIsAutoAllowed(remoteAddr) {
		return
	}

	// 当前连接是否已关闭
	if this.isConnClosed() {
		this.disableLog = true
		return true
	}

	// 是否在全局名单中
	canGoNext, isInAllowedList := iplibrary.AllowIP(remoteAddr, this.ReqServer.Id)
	if !canGoNext {
		this.disableLog = true
		this.Close()
		return true
	}
	if isInAllowedList {
		return false
	}

	// 检查是否在临时黑名单中
	if waf.SharedIPBlackList.Contains(waf.IPTypeAll, firewallconfigs.FirewallScopeService, this.ReqServer.Id, remoteAddr) || waf.SharedIPBlackList.Contains(waf.IPTypeAll, firewallconfigs.FirewallScopeGlobal, 0, remoteAddr) {
		this.disableLog = true
		this.Close()

		return true
	}

	var forceLog = false
	var forceLogRequestBody = false
	var forceLogRegionDenying = false
	if this.ReqServer.HTTPFirewallPolicy != nil &&
		this.ReqServer.HTTPFirewallPolicy.IsOn &&
		this.ReqServer.HTTPFirewallPolicy.Log != nil &&
		this.ReqServer.HTTPFirewallPolicy.Log.IsOn {
		forceLog = true
		forceLogRequestBody = this.ReqServer.HTTPFirewallPolicy.Log.RequestBody
		forceLogRegionDenying = this.ReqServer.HTTPFirewallPolicy.Log.RegionDenying
	}

	// 当前服务的独立设置
	if this.web.FirewallPolicy != nil && this.web.FirewallPolicy.IsOn {
		blocked, breakChecking := this.checkWAFRequest(this.web.FirewallPolicy, forceLog, forceLogRequestBody, forceLogRegionDenying)
		if blocked {
			return true
		}
		if breakChecking {
			return false
		}
	}

	// 公用的防火墙设置
	if this.ReqServer.HTTPFirewallPolicy != nil && this.ReqServer.HTTPFirewallPolicy.IsOn {
		blocked, breakChecking := this.checkWAFRequest(this.ReqServer.HTTPFirewallPolicy, forceLog, forceLogRequestBody, forceLogRegionDenying)
		if blocked {
			return true
		}
		if breakChecking {
			return false
		}
	}

	return
}

func (this *HTTPRequest) checkWAFRequest(firewallPolicy *firewallconfigs.HTTPFirewallPolicy, forceLog bool, logRequestBody bool, logDenying bool) (blocked bool, breakChecking bool) {
	// 检查配置是否为空
	if firewallPolicy == nil || !firewallPolicy.IsOn || firewallPolicy.Inbound == nil || !firewallPolicy.Inbound.IsOn || firewallPolicy.Mode == firewallconfigs.FirewallModeBypass {
		return
	}

	// 检查IP白名单
	var remoteAddrs []string
	if len(this.remoteAddr) > 0 {
		remoteAddrs = []string{this.remoteAddr}
	} else {
		remoteAddrs = this.requestRemoteAddrs()
	}

	var inbound = firewallPolicy.Inbound
	if inbound == nil {
		return
	}
	for _, ref := range inbound.AllAllowListRefs() {
		if ref.IsOn && ref.ListId > 0 {
			list := iplibrary.SharedIPListManager.FindList(ref.ListId)
			if list != nil {
				_, found := list.ContainsIPStrings(remoteAddrs)
				if found {
					breakChecking = true
					return
				}
			}
		}
	}

	// 检查IP黑名单
	if firewallPolicy.Mode == firewallconfigs.FirewallModeDefend {
		for _, ref := range inbound.AllDenyListRefs() {
			if ref.IsOn && ref.ListId > 0 {
				list := iplibrary.SharedIPListManager.FindList(ref.ListId)
				if list != nil {
					item, found := list.ContainsIPStrings(remoteAddrs)
					if found {
						// 触发事件
						if item != nil && len(item.EventLevel) > 0 {
							actions := iplibrary.SharedActionManager.FindEventActions(item.EventLevel)
							for _, action := range actions {
								goNext, err := action.DoHTTP(this.RawReq, this.RawWriter)
								if err != nil {
									remotelogs.Error("HTTP_REQUEST_WAF", "do action '"+err.Error()+"' failed: "+err.Error())
									return true, false
								}
								if !goNext {
									this.disableLog = true
									return true, false
								}
							}
						}

						// TODO 需要记录日志信息

						this.writer.WriteHeader(http.StatusForbidden)
						this.writer.Close()

						// 停止日志
						this.disableLog = true

						return true, false
					}
				}
			}
		}
	}

	// 检查地区封禁
	if firewallPolicy.Mode == firewallconfigs.FirewallModeDefend {
		if firewallPolicy.Inbound.Region != nil && firewallPolicy.Inbound.Region.IsOn {
			regionConfig := firewallPolicy.Inbound.Region
			if regionConfig.IsNotEmpty() {
				for _, remoteAddr := range remoteAddrs {
					var result = iplib.LookupIP(remoteAddr)
					if result != nil && result.IsOk() {
						// 检查国家/地区级别封禁
						var countryId = result.CountryId()
						if countryId > 0 && lists.ContainsInt64(regionConfig.DenyCountryIds, countryId) {
							this.firewallPolicyId = firewallPolicy.Id

							this.writeCode(http.StatusForbidden, "", "")
							this.writer.Flush()
							this.writer.Close()

							// 停止日志
							if !logDenying {
								this.disableLog = true
							} else {
								this.tags = append(this.tags, "denyCountry")
							}

							return true, false
						}

						// 检查省份封禁
						var provinceId = result.ProvinceId()
						if provinceId > 0 && lists.ContainsInt64(regionConfig.DenyProvinceIds, provinceId) {
							this.firewallPolicyId = firewallPolicy.Id

							this.writeCode(http.StatusForbidden, "", "")
							this.writer.Flush()
							this.writer.Close()

							// 停止日志
							if !logDenying {
								this.disableLog = true
							} else {
								this.tags = append(this.tags, "denyProvince")
							}

							return true, false
						}
					}
				}
			}
		}
	}

	// 规则测试
	w := waf.SharedWAFManager.FindWAF(firewallPolicy.Id)
	if w == nil {
		return
	}

	goNext, hasRequestBody, ruleGroup, ruleSet, err := w.MatchRequest(this, this.writer)
	if forceLog && logRequestBody && hasRequestBody && ruleSet != nil && ruleSet.HasAttackActions() {
		this.wafHasRequestBody = true
	}
	if err != nil {
		if !this.canIgnore(err) {
			remotelogs.Error("HTTP_REQUEST_WAF", this.rawURI+": "+err.Error())
		}
		return
	}

	if ruleSet != nil {
		if forceLog {
			this.forceLog = true
		}

		if ruleSet.HasSpecialActions() {
			this.firewallPolicyId = firewallPolicy.Id
			this.firewallRuleGroupId = types.Int64(ruleGroup.Id)
			this.firewallRuleSetId = types.Int64(ruleSet.Id)

			if ruleSet.HasAttackActions() {
				this.isAttack = true
			}

			// 添加统计
			stats.SharedHTTPRequestStatManager.AddFirewallRuleGroupId(this.ReqServer.Id, this.firewallRuleGroupId, ruleSet.Actions)
		}

		this.firewallActions = append(ruleSet.ActionCodes(), firewallPolicy.Mode)
	}

	return !goNext, false
}

// call response waf
func (this *HTTPRequest) doWAFResponse(resp *http.Response) (blocked bool) {
	if this.web.FirewallRef == nil || !this.web.FirewallRef.IsOn {
		return
	}

	// 当前服务的独立设置
	var forceLog = false
	var forceLogRequestBody = false
	if this.ReqServer.HTTPFirewallPolicy != nil && this.ReqServer.HTTPFirewallPolicy.IsOn && this.ReqServer.HTTPFirewallPolicy.Log != nil && this.ReqServer.HTTPFirewallPolicy.Log.IsOn {
		forceLog = true
		forceLogRequestBody = this.ReqServer.HTTPFirewallPolicy.Log.RequestBody
	}

	if this.web.FirewallPolicy != nil && this.web.FirewallPolicy.IsOn {
		blocked := this.checkWAFResponse(this.web.FirewallPolicy, resp, forceLog, forceLogRequestBody)
		if blocked {
			return true
		}
	}

	// 公用的防火墙设置
	if this.ReqServer.HTTPFirewallPolicy != nil && this.ReqServer.HTTPFirewallPolicy.IsOn {
		blocked := this.checkWAFResponse(this.ReqServer.HTTPFirewallPolicy, resp, forceLog, forceLogRequestBody)
		if blocked {
			return true
		}
	}
	return
}

func (this *HTTPRequest) checkWAFResponse(firewallPolicy *firewallconfigs.HTTPFirewallPolicy, resp *http.Response, forceLog bool, logRequestBody bool) (blocked bool) {
	if firewallPolicy == nil || !firewallPolicy.IsOn || !firewallPolicy.Outbound.IsOn || firewallPolicy.Mode == firewallconfigs.FirewallModeBypass {
		return
	}

	w := waf.SharedWAFManager.FindWAF(firewallPolicy.Id)
	if w == nil {
		return
	}

	goNext, hasRequestBody, ruleGroup, ruleSet, err := w.MatchResponse(this, resp, this.writer)
	if forceLog && logRequestBody && hasRequestBody && ruleSet != nil && ruleSet.HasAttackActions() {
		this.wafHasRequestBody = true
	}
	if err != nil {
		if !this.canIgnore(err) {
			remotelogs.Error("HTTP_REQUEST_WAF", this.rawURI+": "+err.Error())
		}
		return
	}

	if ruleSet != nil {
		if forceLog {
			this.forceLog = true
		}

		if ruleSet.HasSpecialActions() {
			this.firewallPolicyId = firewallPolicy.Id
			this.firewallRuleGroupId = types.Int64(ruleGroup.Id)
			this.firewallRuleSetId = types.Int64(ruleSet.Id)

			if ruleSet.HasAttackActions() {
				this.isAttack = true
			}

			// 添加统计
			stats.SharedHTTPRequestStatManager.AddFirewallRuleGroupId(this.ReqServer.Id, this.firewallRuleGroupId, ruleSet.Actions)
		}

		this.firewallActions = append(ruleSet.ActionCodes(), firewallPolicy.Mode)
	}

	return !goNext
}

// WAFRaw 原始请求
func (this *HTTPRequest) WAFRaw() *http.Request {
	return this.RawReq
}

// WAFRemoteIP 客户端IP
func (this *HTTPRequest) WAFRemoteIP() string {
	return this.requestRemoteAddr(true)
}

// WAFGetCacheBody 获取缓存中的Body
func (this *HTTPRequest) WAFGetCacheBody() []byte {
	return this.requestBodyData
}

// WAFSetCacheBody 设置Body
func (this *HTTPRequest) WAFSetCacheBody(body []byte) {
	this.requestBodyData = body
}

// WAFReadBody 读取Body
func (this *HTTPRequest) WAFReadBody(max int64) (data []byte, err error) {
	if this.RawReq.ContentLength > 0 {
		data, err = io.ReadAll(io.LimitReader(this.RawReq.Body, max))
	}

	return
}

// WAFRestoreBody 恢复Body
func (this *HTTPRequest) WAFRestoreBody(data []byte) {
	if len(data) > 0 {
		this.RawReq.Body = io.NopCloser(io.MultiReader(bytes.NewBuffer(data), this.RawReq.Body))
	}
}

// WAFServerId 服务ID
func (this *HTTPRequest) WAFServerId() int64 {
	return this.ReqServer.Id
}

// WAFClose 关闭连接
func (this *HTTPRequest) WAFClose() {
	this.Close()

	// 这里不要强关IP所有连接，避免因为单个服务而影响所有
}

func (this *HTTPRequest) WAFOnAction(action interface{}) (goNext bool) {
	if action == nil {
		return true
	}

	instance, ok := action.(waf.ActionInterface)
	if !ok {
		return true
	}

	switch instance.Code() {
	case waf.ActionTag:
		this.tags = append(this.tags, action.(*waf.TagAction).Tags...)
	}
	return true
}
