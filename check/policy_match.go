package check

import (
	"fmt"
	"encoding/json"
	"github.com/thewayma/suricata_checker/g"
)

func Judge(L *SafeLinkedList, firstItem *g.JudgeItem, now int64) {
	CheckStrategy(L, firstItem, now)
	//CheckExpression(L, firstItem, now)    //!< 暂时不需要expression机制
}

func CheckStrategy(L *SafeLinkedList, firstItem *g.JudgeItem, now int64) {
	key := fmt.Sprintf("%s/%s", firstItem.Endpoint, firstItem.Metric)
	strategyMap := g.StrategyMap.Get()
	strategies, exists := strategyMap[key]
	if !exists {
		return
	}

	for _, s := range strategies {
		// 因为key仅仅是endpoint和metric，所以得到的strategies并不一定是与当前judgeItem相关的
		// 比如lg-dinp-docker01.bj配置了两个proc.num的策略，一个name=docker，一个name=agent
		// 所以此处要排除掉一部分
		related := true
		for tagKey, tagVal := range s.Tags {
			if myVal, exists := firstItem.Tags[tagKey]; !exists || myVal != tagVal {
				related = false
				break
			}
		}

		if !related {
			continue
		}

		judgeItemWithStrategy(L, s, firstItem, now)
	}
}

func judgeItemWithStrategy(L *SafeLinkedList, strategy g.Strategy, firstItem *g.JudgeItem, now int64) {
	fn, err := ParseFuncFromString(strategy.Func, strategy.Operator, strategy.RightValue)
	if err != nil {
		g.Log.Error("[ERROR] parse func %s fail: %v. strategy id: %d", strategy.Func, err, strategy.Id)
		return
	}

	historyData, leftValue, isTriggered, isEnough := fn.Compute(L)
	if !isEnough {
		return
	}

	event := &g.Event{
		Id:         fmt.Sprintf("s_%d_%s", strategy.Id, firstItem.PrimaryKey()),
		Strategy:   &strategy,
		Endpoint:   firstItem.Endpoint,
		LeftValue:  leftValue,
		EventTime:  firstItem.Timestamp,
		PushedTags: firstItem.Tags,
	}

	sendEventIfNeed(historyData, isTriggered, now, event, strategy.MaxStep)
}

func sendEvent(event *g.Event) {
	// update last event
	g.LastEvents.Set(event.Id, event)

	bs, err := json.Marshal(event)
	if err != nil {
		g.Log.Error("json marshal event %v fail: %v", event, err)
		return
	}

	// send to redis
	redisKey := fmt.Sprintf(g.Config().Alarm.QueuePattern, event.Priority())
	rc := g.RedisConnPool.Get()
	defer rc.Close()
	rc.Do("LPUSH", redisKey, string(bs))
}

func buildKeysFromMetricAndTags(item *g.JudgeItem) (keys []string) {
	for k, v := range item.Tags {
		keys = append(keys, fmt.Sprintf("%s/%s=%s", item.Metric, k, v))
	}
	keys = append(keys, fmt.Sprintf("%s/endpoint=%s", item.Metric, item.Endpoint))
	return
}

func copyItemTags(item *g.JudgeItem) map[string]string {
	ret := make(map[string]string)
	ret["endpoint"] = item.Endpoint
	if item.Tags != nil && len(item.Tags) > 0 {
		for k, v := range item.Tags {
			ret[k] = v
		}
	}
	return ret
}

func sendEventIfNeed(historyData []*g.HistoryData, isTriggered bool, now int64, event *g.Event, maxStep int) {
	lastEvent, exists := g.LastEvents.Get(event.Id)
	if isTriggered {
		event.Status = "PROBLEM"
		if !exists || lastEvent.Status[0] == 'O' {
			// 本次触发了阈值，之前又没报过警，得产生一个报警Event
			event.CurrentStep = 1

			// 但是有些用户把最大报警次数配置成了0，相当于屏蔽了，要检查一下
			if maxStep == 0 {
				return
			}

			sendEvent(event)
			return
		}

		// 逻辑走到这里，说明之前Event是PROBLEM状态
		if lastEvent.CurrentStep >= maxStep {
			// 报警次数已经足够多，到达了最多报警次数了，不再报警
			return
		}

		if historyData[len(historyData)-1].Timestamp <= lastEvent.EventTime {
			// 产生过报警的点，就不能再使用来判断了，否则容易出现一分钟报一次的情况
			// 只需要拿最后一个historyData来做判断即可，因为它的时间最老
			return
		}

		if now-lastEvent.EventTime < g.Config().Alarm.MinInterval {
			// 报警不能太频繁，两次报警之间至少要间隔MinInterval秒，否则就不能报警
			return
		}

		event.CurrentStep = lastEvent.CurrentStep + 1
		sendEvent(event)
	} else {
		// 如果LastEvent是Problem，报OK，否则啥都不做
		if exists && lastEvent.Status[0] == 'P' {
			event.Status = "OK"
			event.CurrentStep = 1
			sendEvent(event)
		}
	}
}
