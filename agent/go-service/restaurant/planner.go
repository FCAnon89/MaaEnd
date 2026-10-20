package restaurant

import (
	"fmt"
	"math"
)

type Dish struct {
	ID       string   `json:"id"`
	Category string   `json:"category"`
	Time     int      `json:"time_seconds"`
	Price    int      `json:"price"`
	Affixes  []string `json:"affixes"`
}

type Drink struct {
	ID    string `json:"id"`
	Price int    `json:"price"`
}

type Level struct {
	Level             int      `json:"level"`
	MinimumDishes     int      `json:"minimum_dishes"`
	MaximumDishes     int      `json:"maximum_dishes"`
	MinimumCategories int      `json:"minimum_categories"`
	UsableCategories  []string `json:"usable_categories"`
	MinimumDrinks     int      `json:"minimum_drinks"`
	MaximumDrinks     int      `json:"maximum_drinks"`
}

type Skill struct {
	ID          string  `json:"id"`
	Effect      string  `json:"effect"`
	Category    string  `json:"category"`
	Affix       string  `json:"affix"`
	SpeedBonus  float64 `json:"speed_bonus"`
	UnlockLevel int     `json:"unlock_level"`
}

type Kitchen struct {
	CharacterIDs []string `json:"character_ids"`
}

type Character struct {
	ID          string            `json:"id"`
	Names       map[string]string `json:"names"`
	InnSkillIDs []string          `json:"inn_skill_ids"`
}

type Data struct {
	Levels            []Level           `json:"levels"`
	Dishes            []Dish            `json:"dishes"`
	Drinks            []Drink           `json:"drinks"`
	Characters        []Character       `json:"characters"`
	Skills            []Skill           `json:"skills"`
	AffixNames        map[string]string `json:"affix_names"`
	PopularityTipRate float64           `json:"popularity_tip_rate"`
}

type DishSelection struct {
	DishID       string `json:"dish_id"`
	KitchenTimes [3]int `json:"kitchen_times_seconds"`
}

type MenuPlan struct {
	Dishes []DishSelection `json:"dishes"`
	Drinks []string        `json:"drinks"`
}

// BuildMenuPlan preserves the selected UI order and computes each dish's kitchen-local cooking times.
func BuildMenuPlan(data Data, dishIDs, drinkIDs []string, kitchens [3]Kitchen) (MenuPlan, error) {
	dishByID := make(map[string]Dish, len(data.Dishes))
	for _, dish := range data.Dishes {
		dishByID[dish.ID] = dish
	}
	skillByID := make(map[string]Skill, len(data.Skills))
	for _, skill := range data.Skills {
		skillByID[skill.ID] = skill
	}
	characterByID := make(map[string]Character, len(data.Characters))
	for _, character := range data.Characters {
		characterByID[character.ID] = character
	}
	plan := MenuPlan{Drinks: append([]string(nil), drinkIDs...)}
	for _, dishID := range dishIDs {
		dish, found := dishByID[dishID]
		if !found {
			return MenuPlan{}, fmt.Errorf("unknown selected dish %q", dishID)
		}
		selection := DishSelection{DishID: dishID}
		for kitchenIndex, kitchen := range kitchens {
			var activeSkills []Skill
			for _, characterID := range kitchen.CharacterIDs {
				character, found := characterByID[characterID]
				if !found {
					return MenuPlan{}, fmt.Errorf("unknown kitchen character %q", characterID)
				}
				for _, skillID := range character.InnSkillIDs {
					if skill, found := skillByID[skillID]; found {
						activeSkills = append(activeSkills, skill)
					}
				}
			}
			cookTime, err := CookingTime(dish, activeSkills)
			if err != nil {
				return MenuPlan{}, err
			}
			selection.KitchenTimes[kitchenIndex] = cookTime
		}
		plan.Dishes = append(plan.Dishes, selection)
	}
	return plan, nil
}

// CookingTime applies each matching line's same-effect additive bonuses, then
// multiplies the resulting category and affix factors before rounding up.
func CookingTime(dish Dish, skills []Skill) (int, error) {
	if dish.Time <= 0 {
		return 0, fmt.Errorf("dish %q has non-positive cooking time", dish.ID)
	}
	type effectKey struct {
		kind   string
		target string
	}
	bonuses := make(map[effectKey]float64)
	for _, skill := range skills {
		key := effectKey{}
		switch skill.Effect {
		case "category_speed":
			if skill.Category != dish.Category {
				continue
			}
			key = effectKey{kind: skill.Effect, target: skill.Category}
		case "affix_speed":
			if !contains(dish.Affixes, skill.Affix) {
				continue
			}
			key = effectKey{kind: skill.Effect, target: skill.Affix}
		default:
			continue
		}
		bonuses[key] += skill.SpeedBonus
	}
	multiplier := 1.0
	for _, bonus := range bonuses {
		multiplier *= 1 + bonus
	}
	if multiplier <= 0 {
		return 0, fmt.Errorf("dish %q has non-positive cooking multiplier", dish.ID)
	}
	return int(math.Ceil(float64(dish.Time) / multiplier)), nil
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
