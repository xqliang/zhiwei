package entity

import (
	"sort"
	"strings"

	"zhiwei/internal/ids"
	"zhiwei/internal/repo"
)

// Candidate 召回出的一个候选实体（白名单成员）：Similarity=该段文本与实体的
// 最佳发音相似度，用于排序与 minSim 过滤；EntityID 供 LLM 输出回指（门控校验）。
type Candidate struct {
	EntityID   ids.ID
	Canonical  string
	Kind       string
	Similarity float64
}

// isCJK 判断 rune 是否汉字（召回窗口只对汉字串滑窗）。
func isCJK(r rune) bool { return r >= 0x4E00 && r <= 0x9FFF }

// containsCJK 串里是否含汉字（决定走拼音比对还是拉丁比对）。
func containsCJK(s string) bool {
	for _, r := range s {
		if isCJK(r) {
			return true
		}
	}
	return false
}

// RecallCandidates 对一段转写文本召回 Top-K 候选实体，构成 LLM 合法白名单。
//
// 切片策略：
//   - 汉字连续块按 2..4 字滑窗取子串（中文姓名 2-4 字覆盖绝大多数；1 字太短误召回高）；
//   - ASCII 连续字母数字按整词取（拉丁代号场景，如 Skynet）；
//
// 比对策略：含汉字子串 → NormalizePinyin 对实体 Pinyin；纯拉丁子串 → NormalizeLatin
// 对实体 Metaphone。同一实体保留最高分；sim ≥ minSim 才入围；按 sim 降序取 Top-K。
// 返回空 = 白名单为空（correct stage 跳过该段 LLM 调用——省成本且避免误改）。
func RecallCandidates(text string, entities []repo.Entity, topK int, minSim float64) []Candidate {
	if text == "" || len(entities) == 0 {
		return nil
	}
	if topK <= 0 {
		topK = 5
	}
	// 1) 切子串：汉字块滑窗 + ASCII 词。
	var subs []string
	runes := []rune(text)
	start := -1 // 当前汉字块起点
	flushCJK := func(end int) {
		if start < 0 {
			return
		}
		block := runes[start:end]
		for l := 2; l <= 4 && l <= len(block); l++ {
			for i := 0; i+l <= len(block); i++ {
				subs = append(subs, string(block[i:i+l]))
			}
		}
		start = -1
	}
	var word strings.Builder
	flushWord := func() {
		if word.Len() > 0 {
			subs = append(subs, word.String())
			word.Reset()
		}
	}
	for i, r := range runes {
		switch {
		case isCJK(r):
			flushWord()
			if start < 0 {
				start = i
			}
		case r < 128 && ((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')):
			flushCJK(i)
			word.WriteRune(r)
		default:
			flushWord()
			flushCJK(i)
		}
	}
	flushWord()
	flushCJK(len(runes))
	if len(subs) == 0 {
		return nil
	}

	// 2) 预归一化子串（评审 T6：把 NormalizePinyin/NormalizeLatin 与 containsCJK 判定
	//    从「每实体 × 每子串」的内层循环提到实体循环之前——每个子串只归一化一次，
	//    而非每个实体重算一遍）。含汉字子串归入拼音键，纯拉丁子串归入拉丁键。
	pyKeys := make([]string, 0, len(subs)) // CJK 子串的拼音键
	ltKeys := make([]string, 0, len(subs)) // 纯拉丁子串的拉丁键
	for _, s := range subs {
		if containsCJK(s) {
			if k := NormalizePinyin(s); k != "" {
				pyKeys = append(pyKeys, k)
			}
		} else {
			if k := NormalizeLatin(s); k != "" {
				ltKeys = append(ltKeys, k)
			}
		}
	}

	// 3) 逐实体取最佳子串相似度（实体几十~几百 × 子串几十 = 万级 Similarity 调用，毫秒级）。
	var out []Candidate
	for _, e := range entities {
		var ep, em string
		if e.Pinyin != nil {
			ep = *e.Pinyin
		}
		if e.Metaphone != nil {
			em = *e.Metaphone
		}
		if ep == "" && em == "" {
			continue // 无发音键（脏数据）不参与
		}
		var top float64
		if ep != "" {
			for _, k := range pyKeys {
				if sim := Similarity(k, ep); sim > top {
					top = sim
				}
			}
		}
		if em != "" {
			for _, k := range ltKeys {
				if sim := Similarity(k, em); sim > top {
					top = sim
				}
			}
		}
		if top >= minSim {
			out = append(out, Candidate{EntityID: e.ID, Canonical: e.Canonical, Kind: e.Kind, Similarity: top})
		}
	}
	// 4) 排序 + Top-K（每实体至多一条，天然无重复）。
	sort.Slice(out, func(i, j int) bool { return out[i].Similarity > out[j].Similarity })
	if len(out) > topK {
		out = out[:topK]
	}
	return out
}
