// Package profile 实现用户画像（人物系统）的领域逻辑：属性目录、
// LLM 抽取、置信度闸门、人物归属解析与落库编排。
// 设计规格：docs/superpowers/specs/2026-08-24-person-profile-system-design.md。
package profile

// AttrDef 是属性目录里一个已知字段的定义。目录外的 key 仍可用
// （Def 返回默认定义，归「其他」组），保证「所有个人信息」可自由扩展。
type AttrDef struct {
	Key         string
	Label       string   // 中文名（表单/展示用）
	Group       string   // 分组名（GroupOrder 之一）
	ValueType   string   // text|enum|bool|date|number（值的类型；列表与否看 Cardinality）
	EnumOptions []string // ValueType=enum 时的取值集
	Cardinality string   // single=同 key 至多一行 active | list=同 key 多行 active（每元素一行）
}

// GroupOrder 属性分组展示顺序（人物详情页分区用）。
var GroupOrder = []string{"基本", "工作", "生活习惯", "兴趣", "出行物品", "关注性格", "健康", "其他"}

// Cardinality 取值。
const (
	CardinalitySingle = "single"
	CardinalityList   = "list"
)

// ValueType 取值（值的类型；列表与否看 Cardinality）。
const (
	ValueTypeText   = "text"
	ValueTypeEnum   = "enum"
	ValueTypeBool   = "bool"
	ValueTypeDate   = "date"
	ValueTypeNumber = "number"
)

func def(key, label, group, vt, card string, enum ...string) AttrDef {
	return AttrDef{Key: key, Label: label, Group: group, ValueType: vt, EnumOptions: enum, Cardinality: card}
}

// catalog 是已知属性全集（spec §4.9 的字段映射）。
var catalog = []AttrDef{
	// ---- 基本 ----
	def("aliases", "别名", "基本", ValueTypeText, CardinalityList),
	def("birthday", "生日", "基本", ValueTypeDate, CardinalitySingle),
	def("gender", "性别", "基本", ValueTypeEnum, CardinalitySingle, "男", "女", "其他"),
	def("zodiac", "星座", "基本", ValueTypeEnum, CardinalitySingle,
		"白羊座", "金牛座", "双子座", "巨蟹座", "狮子座", "处女座",
		"天秤座", "天蝎座", "射手座", "摩羯座", "水瓶座", "双鱼座"),
	def("mbti", "MBTI", "基本", ValueTypeEnum, CardinalitySingle,
		"INTJ", "INTP", "ENTJ", "ENTP", "INFJ", "INFP", "ENFJ", "ENFP",
		"ISTJ", "ISFJ", "ESTJ", "ESFJ", "ISTP", "ISFP", "ESTP", "ESFP"),
	def("education", "学历", "基本", ValueTypeEnum, CardinalitySingle, "高中及以下", "大专", "本科", "硕士", "博士"),
	def("school", "学校", "基本", ValueTypeText, CardinalityList),
	def("city", "城市", "基本", ValueTypeText, CardinalitySingle),
	def("address", "住址", "基本", ValueTypeText, CardinalitySingle),
	def("phone", "手机号", "基本", ValueTypeText, CardinalitySingle),

	// ---- 工作 ----
	def("occupation", "职业", "工作", ValueTypeText, CardinalitySingle),
	def("industry", "所属行业", "工作", ValueTypeText, CardinalitySingle),
	def("office_location", "办公地点", "工作", ValueTypeText, CardinalitySingle),
	def("work_start_time", "上班时间", "工作", ValueTypeText, CardinalitySingle),
	def("work_end_time", "下班时间", "工作", ValueTypeText, CardinalitySingle),
	def("commute_mode", "通勤方式", "工作", ValueTypeEnum, CardinalitySingle,
		"步行", "自行车", "电动车", "地铁", "公交", "开车", "打车", "班车", "火车", "高铁", "飞机"),
	def("often_travel", "是否经常出差", "工作", ValueTypeBool, CardinalitySingle),
	def("current_projects", "正在进行的项目", "工作", ValueTypeText, CardinalityList),

	// ---- 生活习惯 ----
	def("meal_time", "吃饭时间", "生活习惯", ValueTypeText, CardinalitySingle),
	def("cuisine", "喜欢的菜系", "生活习惯", ValueTypeEnum, CardinalityList,
		"川菜", "粤菜", "湘菜", "鲁菜", "苏菜", "浙菜", "闽菜", "徽菜",
		"火锅", "烧烤", "西餐", "日料", "韩餐", "家常菜"),
	def("eats_spicy", "是否吃辣", "生活习惯", ValueTypeBool, CardinalitySingle),
	def("eats_numbing", "是否吃麻", "生活习惯", ValueTypeBool, CardinalitySingle),
	def("smokes", "是否吸烟", "生活习惯", ValueTypeBool, CardinalitySingle),
	def("drinks", "是否喝酒", "生活习惯", ValueTypeBool, CardinalitySingle),
	def("wears_makeup", "是否化妆", "生活习惯", ValueTypeBool, CardinalitySingle),
	def("perfume", "香水", "生活习惯", ValueTypeText, CardinalitySingle),

	// ---- 兴趣 ----
	def("hobbies", "爱好", "兴趣", ValueTypeText, CardinalityList),
	def("skills", "学的技能", "兴趣", ValueTypeText, CardinalityList),
	def("reading_now", "正在看的书", "兴趣", ValueTypeText, CardinalityList),
	def("books_read", "看过的书", "兴趣", ValueTypeText, CardinalityList),
	def("movies_watched", "看过的影视", "兴趣", ValueTypeText, CardinalityList),
	def("music_listened", "听过的音乐", "兴趣", ValueTypeText, CardinalityList),
	def("games_played", "玩过的游戏", "兴趣", ValueTypeText, CardinalityList),
	def("fav_celebrities", "喜欢的明星", "兴趣", ValueTypeText, CardinalityList),
	def("fav_anime", "喜欢的动漫", "兴趣", ValueTypeText, CardinalityList),
	def("fav_movie_genres", "喜欢的电影类型", "兴趣", ValueTypeText, CardinalityList),
	def("catchphrases", "口头禅", "兴趣", ValueTypeText, CardinalityList),
	def("invests_stocks", "是否炒股", "兴趣", ValueTypeBool, CardinalitySingle),

	// ---- 出行物品 ----
	def("cities_visited", "去过的城市", "出行物品", ValueTypeText, CardinalityList),
	def("places_traveled", "旅游过的地方", "出行物品", ValueTypeText, CardinalityList),
	def("has_car", "是否有车", "出行物品", ValueTypeBool, CardinalitySingle),
	def("car_brand", "车品牌", "出行物品", ValueTypeText, CardinalitySingle),
	def("phone_brand", "手机品牌", "出行物品", ValueTypeText, CardinalitySingle),

	// ---- 关注性格 ----
	def("recent_concerns", "最近关心的事情", "关注性格", ValueTypeText, CardinalityList),
	def("attention_topics", "关注领域", "关注性格", ValueTypeEnum, CardinalityList,
		"政治", "军事", "体育", "三农", "科技", "财经", "娱乐", "教育", "健康"),
	def("personality", "性格", "关注性格", ValueTypeText, CardinalitySingle),

	// ---- 健康（P3 深化，先占位属性） ----
	def("chronic_diseases", "慢性病", "健康", ValueTypeText, CardinalityList),
}

var catalogMap = func() map[string]AttrDef {
	m := make(map[string]AttrDef, len(catalog))
	for _, d := range catalog {
		m[d.Key] = d
	}
	return m
}()

// Def 返回 key 的目录定义；目录外 key 返回「其他」组的默认定义（text/single），可自由扩展。
func Def(key string) AttrDef {
	if d, ok := catalogMap[key]; ok {
		return d
	}
	return AttrDef{Key: key, Label: key, Group: "其他", ValueType: ValueTypeText, Cardinality: CardinalitySingle}
}

// IsList 判断 key 是否列表型属性。
func IsList(key string) bool { return Def(key).Cardinality == CardinalityList }

// All 返回全部目录定义（目录顺序即分组顺序）。
func All() []AttrDef { return catalog }
