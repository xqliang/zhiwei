package profile

import "testing"

func TestCatalogDefs(t *testing.T) {
	// 已知 key：完整定义
	d := Def("occupation")
	if d.Label != "职业" || d.Group != "工作" || d.Cardinality != CardinalitySingle || d.ValueType != "text" {
		t.Fatalf("occupation 定义错误: %+v", d)
	}
	// 枚举型
	g := Def("gender")
	if g.ValueType != "enum" || len(g.EnumOptions) != 3 {
		t.Fatalf("gender 定义错误: %+v", g)
	}
	// 列表型
	h := Def("hobbies")
	if !IsList("hobbies") || h.Group != "兴趣" {
		t.Fatalf("hobbies 应为列表型: %+v", h)
	}
	// 目录外 key：默认「其他」组、text、single
	u := Def("custom_key_xyz")
	if u.Group != "其他" || u.ValueType != "text" || u.Cardinality != CardinalitySingle {
		t.Fatalf("未知 key 默认定义错误: %+v", u)
	}
	// 分组顺序：GroupOrder 覆盖所有目录里出现过的分组
	seen := map[string]bool{}
	for _, d := range All() {
		seen[d.Group] = true
	}
	for _, g := range GroupOrder {
		if g != "其他" {
			delete(seen, g)
		}
	}
	if len(seen) > 1 { // 只允许剩「其他」（目录内不显式用它）
		t.Fatalf("有分组未列入 GroupOrder: %v", seen)
	}
	// key 不重复
	keys := map[string]bool{}
	for _, d := range All() {
		if keys[d.Key] {
			t.Fatalf("目录 key 重复: %s", d.Key)
		}
		keys[d.Key] = true
	}
	// 需求字段抽查（用户原始清单的关键字段必须都在）
	for _, k := range []string{"aliases", "birthday", "gender", "zodiac", "mbti", "education",
		"school", "city", "address", "phone", "occupation", "industry", "office_location",
		"work_start_time", "work_end_time", "commute_mode", "often_travel", "current_projects",
		"meal_time", "cuisine", "eats_spicy", "eats_numbing", "smokes", "drinks",
		"wears_makeup", "perfume", "hobbies", "skills", "reading_now", "books_read",
		"movies_watched", "music_listened", "games_played", "fav_celebrities", "fav_anime",
		"fav_movie_genres", "catchphrases", "invests_stocks", "cities_visited", "places_traveled",
		"has_car", "car_brand", "phone_brand", "recent_concerns", "attention_topics",
		"personality", "chronic_diseases"} {
		if _, ok := catalogMap[k]; !ok {
			t.Errorf("需求字段缺失于目录: %s", k)
		}
	}
}
