package service

import "encoding/json"

func intsJSON(ids []int) string {
	b, _ := json.Marshal(ids)
	return string(b)
}

func jsonInts(s string) []int {
	var ids []int
	if err := json.Unmarshal([]byte(s), &ids); err != nil {
		return nil
	}
	return ids
}
