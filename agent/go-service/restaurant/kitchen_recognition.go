package restaurant

import (
	"encoding/json"
	"fmt"
	"strings"

	maa "github.com/MaaXYZ/maa-framework-go/v4"
	"github.com/rs/zerolog/log"
)

// KitchenOperatorsRecognition records the two cooks actually chosen in the
// picker, then verifies the same pair after Pipeline confirms that kitchen.
type KitchenOperatorsRecognition struct{}

var _ maa.CustomRecognitionRunner = (*KitchenOperatorsRecognition)(nil)

type kitchenOperatorsParam struct {
	Kitchen int    `json:"kitchen"`
	View    string `json:"view"`
}

func (r *KitchenOperatorsRecognition) Run(ctx *maa.Context, arg *maa.CustomRecognitionArg) (*maa.CustomRecognitionResult, bool) {
	if ctx == nil || arg == nil || arg.Img == nil {
		return nil, false
	}
	var param kitchenOperatorsParam
	if err := json.Unmarshal([]byte(arg.CustomRecognitionParam), &param); err != nil || param.Kitchen < 1 || param.Kitchen > 3 || (param.View != "picker" && param.View != "setup") {
		return nil, false
	}
	data, err := loadRestaurantData()
	if err != nil {
		return nil, false
	}
	kitchens, _, err := readAttach[[3]Kitchen](ctx, restaurantPlanNode, kitchensAttach)
	if err != nil {
		return nil, false
	}
	rois := [2]maa.Rect{{500, 116, 180, 30}, {920, 116, 180, 30}}
	if param.View == "setup" {
		x := 560 + (param.Kitchen-1)*225
		rois = [2]maa.Rect{{x, 333, 95, 30}, {x + 86, 333, 98, 30}}
	}
	ids := make([]string, 0, 2)
	for _, roi := range rois {
		texts, err := recognizeOCR(ctx, arg.Img, roi)
		if err != nil {
			return nil, false
		}
		id, err := kitchenCharacterID(data.Characters, joinOCR(texts))
		if err != nil {
			log.Debug().Err(err).Int("kitchen", param.Kitchen).Str("view", param.View).Msg("kitchen cook name is not ready")
			return nil, false
		}
		ids = append(ids, id)
	}
	if ids[0] == ids[1] {
		return nil, false
	}
	if param.View == "picker" {
		for index, other := range kitchens {
			if index == param.Kitchen-1 {
				continue
			}
			if contains(other.CharacterIDs, ids[0]) || contains(other.CharacterIDs, ids[1]) {
				return nil, false
			}
		}
		kitchens[param.Kitchen-1] = Kitchen{CharacterIDs: ids}
		if err := writeAttach(ctx, restaurantPlanNode, kitchensAttach, kitchens); err != nil {
			return nil, false
		}
	} else {
		expected := kitchens[param.Kitchen-1].CharacterIDs
		if len(expected) != 2 || expected[0] != ids[0] || expected[1] != ids[1] {
			return nil, false
		}
	}
	return &maa.CustomRecognitionResult{Box: rois[0], Detail: strings.Join(ids, ",")}, true
}

func kitchenCharacterID(characters []Character, text string) (string, error) {
	name := strings.TrimPrefix(strings.Join(strings.Fields(text), ""), "大厨")
	var selected *Character
	for index := range characters {
		character := &characters[index]
		if character.Names["CN"] != name {
			continue
		}
		if selected != nil && strings.Join(selected.InnSkillIDs, ",") != strings.Join(character.InnSkillIDs, ",") {
			return "", fmt.Errorf("cook name %q has ambiguous skill data", text)
		}
		if selected == nil {
			selected = character
		}
	}
	if selected == nil {
		return "", fmt.Errorf("unknown cook name %q", text)
	}
	return selected.ID, nil
}
