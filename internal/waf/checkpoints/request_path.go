package checkpoints

import (
	"github.com/TeaOSLab/EdgeNode/internal/waf/requests"
	"github.com/iwind/TeaGo/maps"
)

type RequestPathCheckpoint struct {
	Checkpoint
}

func (this *RequestPathCheckpoint) RequestValue(req requests.Request, param string, options maps.Map, ruleId int64) (value interface{}, hasRequestBody bool, sysErr error, userErr error) {
	return req.WAFRaw().URL.Path, false, nil, nil
}

func (this *RequestPathCheckpoint) ResponseValue(req requests.Request, resp *requests.Response, param string, options maps.Map, ruleId int64) (value interface{}, hasRequestBody bool, sysErr error, userErr error) {
	if this.IsRequest() {
		return this.RequestValue(req, param, options, ruleId)
	}
	return
}
