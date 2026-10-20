package restaurant

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	maa "github.com/MaaXYZ/maa-framework-go/v4"
	"github.com/rs/zerolog/log"
)

const (
	menuRecognitionComponent = "RestaurantMenuRecognition"
	menuCardWidth            = 307
	menuCardHeight           = 93
	menuPlanAttach           = "plan"
	menuPlanErrorAttach      = "error"
	menuSelectionAttach      = "state"
)

var (
	menuSizeRequirementPatterns = []*regexp.Regexp{
		regexp.MustCompile(`至少需要(\d+)道菜品和(\d+)种饮品`),
		regexp.MustCompile(`Atleast(\d+)dish\(es\)and(\d+)drink\(s\)`),
		regexp.MustCompile(`料理：(\d+)品以上、飲み物：(\d+)種類以上`),
		regexp.MustCompile(`최소(\d+)개의요리와최소(\d+)종의음료가필요합니다\.`),
		regexp.MustCompile(`至少需要(\d+)道菜品和(\d+)種飲品`),
	}
	menuCardsX = [2]int{552, 870}
)

type MenuInitializeRecognition struct{}

var _ maa.CustomRecognitionRunner = (*MenuInitializeRecognition)(nil)

func (r *MenuInitializeRecognition) Run(ctx *maa.Context, arg *maa.CustomRecognitionArg) (*maa.CustomRecognitionResult, bool) {
	if ctx == nil || arg == nil {
		return nil, false
	}
	for nodeName, values := range map[string]map[string]any{
		restaurantPlanNode:       {menuPlanAttach: nil, menuPlanErrorAttach: "", kitchensAttach: [3]Kitchen{}},
		restaurantMenuSelectNode: {menuSelectionAttach: menuSelectionState{}},
		restaurantRuntimeNode:    {runtimeStateAttach: nil},
	} {
		for key, value := range values {
			if err := writeAttach(ctx, nodeName, key, value); err != nil {
				log.Error().Err(err).Str("component", menuRecognitionComponent).Str("node", nodeName).Msg("failed to initialize restaurant state")
				return nil, false
			}
		}
	}
	for _, nodeName := range restaurantScrollNodes {
		if err := writeAttach(ctx, nodeName, "ready", false); err != nil {
			log.Error().Err(err).Str("component", menuRecognitionComponent).Str("node", nodeName).Msg("failed to reset menu scroll state")
			return nil, false
		}
	}
	return &maa.CustomRecognitionResult{Box: maa.Rect{0, 0, 1280, 720}}, true
}

type menuKindParam struct {
	Kind string `json:"kind"`
}

type MenuPlanRecognition struct{}

var _ maa.CustomRecognitionRunner = (*MenuPlanRecognition)(nil)

func (r *MenuPlanRecognition) Run(ctx *maa.Context, arg *maa.CustomRecognitionArg) (*maa.CustomRecognitionResult, bool) {
	if ctx == nil || arg == nil || arg.Img == nil {
		return nil, false
	}
	fail := func(err error) (*maa.CustomRecognitionResult, bool) {
		if writeErr := writeAttach(ctx, restaurantPlanNode, menuPlanErrorAttach, err.Error()); writeErr != nil {
			log.Error().Err(writeErr).Str("component", menuRecognitionComponent).Msg("failed to store menu preparation error")
			return nil, false
		}
		return &maa.CustomRecognitionResult{Box: maa.Rect{85, 145, 395, 28}, Detail: err.Error()}, true
	}
	texts, err := recognizeOCR(ctx, arg.Img, maa.Rect{121, 171, 367, 58})
	if err != nil {
		return fail(fmt.Errorf("read menu requirement: %w", err))
	}
	requirements, err := parseMenuRequirements(joinOCR(texts))
	if err != nil {
		return fail(err)
	}
	kitchens, found, err := readAttach[[3]Kitchen](ctx, restaurantPlanNode, kitchensAttach)
	if err != nil || !found {
		return fail(fmt.Errorf("chosen kitchen cooks are missing"))
	}
	for index, kitchen := range kitchens {
		if len(kitchen.CharacterIDs) != 2 {
			return fail(fmt.Errorf("kitchen %d does not have two verified cooks", index+1))
		}
	}
	if err := writeAttach(ctx, restaurantPlanNode, menuPlanAttach, nil); err != nil {
		return fail(err)
	}
	state := menuSelectionState{MinimumDishes: requirements.MinimumDishes, MinimumDrinks: requirements.MinimumDrinks}
	if err := writeAttach(ctx, restaurantMenuSelectNode, menuSelectionAttach, state); err != nil {
		return fail(fmt.Errorf("initialize menu selection: %w", err))
	}
	if err := writeAttach(ctx, restaurantPlanNode, menuPlanErrorAttach, ""); err != nil {
		return nil, false
	}
	return &maa.CustomRecognitionResult{Box: maa.Rect{85, 145, 395, 28}}, true
}

type MenuPlanFailureRecognition struct{}

var _ maa.CustomRecognitionRunner = (*MenuPlanFailureRecognition)(nil)

func (r *MenuPlanFailureRecognition) Run(ctx *maa.Context, arg *maa.CustomRecognitionArg) (*maa.CustomRecognitionResult, bool) {
	if ctx == nil || arg == nil {
		return nil, false
	}
	planErr, found, err := readAttach[string](ctx, restaurantPlanNode, menuPlanErrorAttach)
	if err != nil {
		return &maa.CustomRecognitionResult{Box: maa.Rect{85, 145, 395, 28}, Detail: err.Error()}, true
	}
	if !found || planErr == "" {
		return nil, false
	}
	return &maa.CustomRecognitionResult{Box: maa.Rect{85, 145, 395, 28}, Detail: planErr}, true
}

type MenuNextSelectionRecognition struct{}

var _ maa.CustomRecognitionRunner = (*MenuNextSelectionRecognition)(nil)

type menuSelectionState struct {
	MinimumDishes int      `json:"minimum_dishes"`
	MinimumDrinks int      `json:"minimum_drinks"`
	Dishes        []string `json:"dishes"`
	Drinks        []string `json:"drinks"`
	PendingID     string   `json:"pending_id"`
	PendingKind   string   `json:"pending_kind"`
	Error         string   `json:"error,omitempty"`
}

func (r *MenuNextSelectionRecognition) Run(ctx *maa.Context, arg *maa.CustomRecognitionArg) (*maa.CustomRecognitionResult, bool) {
	if ctx == nil || arg == nil || arg.Img == nil {
		return nil, false
	}
	var param menuKindParam
	if err := json.Unmarshal([]byte(arg.CustomRecognitionParam), &param); err != nil || (param.Kind != "dish" && param.Kind != "drink") {
		return nil, false
	}
	state, found, err := readAttach[menuSelectionState](ctx, restaurantMenuSelectNode, menuSelectionAttach)
	if err != nil {
		return r.fail(ctx, err)
	}
	if !found || state.MinimumDishes <= 0 {
		return r.fail(ctx, fmt.Errorf("menu requirements are missing"))
	}
	if state.Error != "" {
		return nil, false
	}
	if state.PendingID != "" {
		return r.fail(ctx, fmt.Errorf("menu item %q is awaiting selection verification", state.PendingID))
	}
	selected, required := state.Dishes, state.MinimumDishes
	if param.Kind == "drink" {
		selected, required = state.Drinks, state.MinimumDrinks
	}
	if len(selected) >= required {
		return nil, false
	}
	data, err := loadRestaurantData()
	if err != nil {
		return r.fail(ctx, err)
	}
	var itemIDs []string
	if param.Kind == "dish" {
		for _, dish := range data.Dishes {
			itemIDs = append(itemIDs, dish.ID)
		}
	} else {
		for _, drink := range data.Drinks {
			itemIDs = append(itemIDs, drink.ID)
		}
	}
	cards, err := visibleMenuCards(ctx, arg.Img, param.Kind)
	if err != nil {
		return r.fail(ctx, err)
	}
	for _, card := range cards {
		if len(selected) >= required {
			break
		}
		if !card.Available {
			continue
		}
		id, err := matchIcon(ctx, arg.Img, card.Icon, itemIDs)
		if err != nil {
			return r.fail(ctx, err)
		}
		if id == "" {
			return r.fail(ctx, fmt.Errorf("unrecognized menu selection icon at %v", card.Box))
		}
		if isMenuCardSelected(arg.Img, card.Box) {
			selected = appendUnique(selected, id)
			if param.Kind == "dish" {
				state.Dishes = selected
			} else {
				state.Drinks = selected
			}
			if err := writeAttach(ctx, restaurantMenuSelectNode, menuSelectionAttach, state); err != nil {
				return r.fail(ctx, err)
			}
			continue
		}
		if contains(selected, id) {
			return r.fail(ctx, fmt.Errorf("verified menu selection %q disappeared", id))
		}
		state.PendingID = id
		state.PendingKind = param.Kind
		if err := writeAttach(ctx, restaurantMenuSelectNode, menuSelectionAttach, state); err != nil {
			return r.fail(ctx, err)
		}
		// The icon opens item details; select from the card's right half.
		target := maa.Rect{card.Box[0] + card.Box[2]*3/4, card.Box[1] + card.Box[3]/2, 1, 1}
		return &maa.CustomRecognitionResult{Box: target, Detail: id}, true
	}
	return nil, false
}

func (r *MenuNextSelectionRecognition) fail(ctx *maa.Context, err error) (*maa.CustomRecognitionResult, bool) {
	state, _, readErr := readAttach[menuSelectionState](ctx, restaurantMenuSelectNode, menuSelectionAttach)
	if readErr != nil {
		err = fmt.Errorf("%v; read selection state: %w", err, readErr)
	}
	state.Error = err.Error()
	if writeErr := writeAttach(ctx, restaurantMenuSelectNode, menuSelectionAttach, state); writeErr != nil {
		log.Error().Err(writeErr).Str("component", menuRecognitionComponent).Msg("failed to store menu selection error")
		return nil, false
	}
	log.Error().Err(err).Str("component", menuRecognitionComponent).Msg("menu selection paused")
	return nil, false
}

// MenuSelectionVerifiedRecognition confirms the just-clicked menu card acquired its selected badge.
type MenuSelectionVerifiedRecognition struct{}

var _ maa.CustomRecognitionRunner = (*MenuSelectionVerifiedRecognition)(nil)

func (r *MenuSelectionVerifiedRecognition) Run(ctx *maa.Context, arg *maa.CustomRecognitionArg) (*maa.CustomRecognitionResult, bool) {
	if ctx == nil || arg == nil || arg.Img == nil {
		return nil, false
	}
	var param menuKindParam
	if err := json.Unmarshal([]byte(arg.CustomRecognitionParam), &param); err != nil || (param.Kind != "dish" && param.Kind != "drink") {
		return r.fail(ctx, fmt.Errorf("invalid menu verification kind"))
	}
	state, found, err := readAttach[menuSelectionState](ctx, restaurantMenuSelectNode, menuSelectionAttach)
	if err != nil || !found || state.PendingID == "" || state.PendingKind != param.Kind {
		return r.fail(ctx, fmt.Errorf("pending menu selection is missing"))
	}
	cards, err := visibleMenuCards(ctx, arg.Img, param.Kind)
	if err != nil {
		return nil, false
	}
	for _, card := range cards {
		id, err := matchIcon(ctx, arg.Img, card.Icon, []string{state.PendingID})
		if err != nil {
			return nil, false
		}
		if id != state.PendingID {
			continue
		}
		if !isMenuCardSelected(arg.Img, card.Box) {
			return nil, false
		}
		if param.Kind == "dish" {
			state.Dishes = appendUnique(state.Dishes, id)
		} else {
			state.Drinks = appendUnique(state.Drinks, id)
		}
		state.PendingID = ""
		state.PendingKind = ""
		if err := writeAttach(ctx, restaurantMenuSelectNode, menuSelectionAttach, state); err != nil {
			return r.fail(ctx, err)
		}
		return &maa.CustomRecognitionResult{Box: card.Icon, Detail: id}, true
	}
	return nil, false
}

func (r *MenuSelectionVerifiedRecognition) fail(ctx *maa.Context, err error) (*maa.CustomRecognitionResult, bool) {
	state, _, readErr := readAttach[menuSelectionState](ctx, restaurantMenuSelectNode, menuSelectionAttach)
	if readErr != nil {
		err = fmt.Errorf("%v; read selection state: %w", err, readErr)
	}
	state.Error = err.Error()
	if writeErr := writeAttach(ctx, restaurantMenuSelectNode, menuSelectionAttach, state); writeErr != nil {
		log.Error().Err(writeErr).Str("component", menuRecognitionComponent).Msg("failed to store menu selection verification error")
		return nil, false
	}
	log.Error().Err(err).Str("component", menuRecognitionComponent).Msg("menu selection verification paused")
	return &maa.CustomRecognitionResult{Box: maa.Rect{551, 133, 641, 403}, Detail: err.Error()}, true
}

// MenuSelectionCompleteRecognition checks the requested count and prepares the ledger after both kinds are selected.
type MenuSelectionCompleteRecognition struct{}

var _ maa.CustomRecognitionRunner = (*MenuSelectionCompleteRecognition)(nil)

func (r *MenuSelectionCompleteRecognition) Run(ctx *maa.Context, arg *maa.CustomRecognitionArg) (*maa.CustomRecognitionResult, bool) {
	if ctx == nil || arg == nil {
		return nil, false
	}
	var param menuKindParam
	if err := json.Unmarshal([]byte(arg.CustomRecognitionParam), &param); err != nil || (param.Kind != "dish" && param.Kind != "drink") {
		return nil, false
	}
	state, found, err := readAttach[menuSelectionState](ctx, restaurantMenuSelectNode, menuSelectionAttach)
	if err != nil || !found || state.Error != "" || state.PendingID != "" || state.MinimumDishes <= 0 {
		return nil, false
	}
	if len(state.Dishes) < state.MinimumDishes {
		return nil, false
	}
	if param.Kind == "drink" {
		if len(state.Drinks) < state.MinimumDrinks {
			return nil, false
		}
		fail := func(err error) (*maa.CustomRecognitionResult, bool) {
			return (&MenuNextSelectionRecognition{}).fail(ctx, err)
		}
		data, err := loadRestaurantData()
		if err != nil {
			return fail(err)
		}
		kitchens, found, err := readAttach[[3]Kitchen](ctx, restaurantPlanNode, kitchensAttach)
		if err != nil || !found {
			return fail(fmt.Errorf("chosen kitchen cooks are missing"))
		}
		plan, err := BuildMenuPlan(data, state.Dishes, state.Drinks, kitchens)
		if err != nil {
			return fail(err)
		}
		ledger, err := NewLedger(plan)
		if err != nil {
			return fail(err)
		}
		if err := writeAttach(ctx, restaurantPlanNode, menuPlanAttach, plan); err != nil {
			return fail(err)
		}
		if err := saveRuntimeState(ctx, runtimeState{Ledger: *ledger}); err != nil {
			return fail(err)
		}
	}
	return &maa.CustomRecognitionResult{Box: maa.Rect{551, 133, 641, 403}, Detail: param.Kind}, true
}

// MenuSelectionFailureRecognition stops the menu loop after an uncertain selection.
type MenuSelectionFailureRecognition struct{}

var _ maa.CustomRecognitionRunner = (*MenuSelectionFailureRecognition)(nil)

func (r *MenuSelectionFailureRecognition) Run(ctx *maa.Context, arg *maa.CustomRecognitionArg) (*maa.CustomRecognitionResult, bool) {
	if ctx == nil || arg == nil {
		return nil, false
	}
	state, found, err := readAttach[menuSelectionState](ctx, restaurantMenuSelectNode, menuSelectionAttach)
	if err != nil {
		return &maa.CustomRecognitionResult{Box: maa.Rect{551, 133, 641, 403}, Detail: err.Error()}, true
	}
	if found && state.Error != "" {
		return &maa.CustomRecognitionResult{Box: maa.Rect{551, 133, 641, 403}, Detail: state.Error}, true
	}
	return nil, false
}

// MenuSelectionBottomRecognition stops when the list ends before the requested count is selected.
type MenuSelectionBottomRecognition struct{}

var _ maa.CustomRecognitionRunner = (*MenuSelectionBottomRecognition)(nil)

func (r *MenuSelectionBottomRecognition) Run(ctx *maa.Context, arg *maa.CustomRecognitionArg) (*maa.CustomRecognitionResult, bool) {
	if ctx == nil || arg == nil {
		return nil, false
	}
	err := fmt.Errorf("the menu list ended before the required number of items was selected")
	_, _ = (&MenuNextSelectionRecognition{}).fail(ctx, err)
	// This node has no action; a hit is needed to reach its FailedNode route.
	return &maa.CustomRecognitionResult{Box: maa.Rect{551, 133, 641, 403}, Detail: err.Error()}, true
}

func appendUnique(values []string, value string) []string {
	if contains(values, value) {
		return values
	}
	return append(values, value)
}

func parseMenuRequirements(text string) (Level, error) {
	text = strings.Join(strings.Fields(text), "")
	var sizes []string
	for _, pattern := range menuSizeRequirementPatterns {
		if sizes = pattern.FindStringSubmatch(text); len(sizes) == 3 {
			break
		}
	}
	if len(sizes) != 3 {
		return Level{}, fmt.Errorf("could not read the visible menu quantities from %q", text)
	}
	requirements := Level{}
	requirements.MinimumDishes, _ = strconv.Atoi(sizes[1])
	requirements.MinimumDrinks, _ = strconv.Atoi(sizes[2])
	if requirements.MinimumDishes <= 0 || requirements.MinimumDrinks < 0 {
		return Level{}, fmt.Errorf("invalid menu quantities in %q", text)
	}
	return requirements, nil
}

func joinOCR(texts []ocrText) string {
	var builder strings.Builder
	for _, item := range texts {
		builder.WriteString(strings.TrimSpace(item.Text))
	}
	return builder.String()
}

func ocrContains(texts []ocrText, fragment string) bool {
	return strings.Contains(joinOCR(texts), fragment)
}

func ocrContainsAny(texts []ocrText, fragments ...string) bool {
	for _, fragment := range fragments {
		if ocrContains(texts, fragment) {
			return true
		}
	}
	return false
}
