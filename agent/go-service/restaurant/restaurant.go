package restaurant

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/MaaXYZ/MaaEnd/agent/go-service/pkg/iconrecognition"
	"github.com/MaaXYZ/MaaEnd/agent/go-service/pkg/resource"
	maa "github.com/MaaXYZ/maa-framework-go/v4"
	"github.com/rs/zerolog/log"
)

// DecisionAction consumes Pipeline observations and updates menu/dispatch decisions.
// It never captures images, runs recognitions, clicks, or advances the UI itself.
type DecisionAction struct{}

var _ maa.CustomActionRunner = &DecisionAction{}

type decisionParam struct {
	Step    string `json:"step"`
	Kind    string `json:"kind"`
	Kitchen int    `json:"kitchen"`
	View    string `json:"view"`
}

type session struct {
	Kitchens [3]Kitchen    `json:"kitchens"`
	Menu     menuState     `json:"menu"`
	Runtime  *runtimeState `json:"runtime,omitempty"`
}

type menuState struct {
	MinimumDishes int        `json:"minimum_dishes"`
	MinimumDrinks int        `json:"minimum_drinks"`
	Dishes        []string   `json:"dishes"`
	Drinks        []string   `json:"drinks"`
	Kind          string     `json:"kind"`
	Cards         []maa.Rect `json:"cards"`
	Index         int        `json:"index"`
	PendingID     string     `json:"pending_id"`
}

type runtimeState struct {
	Ledger            Ledger `json:"ledger"`
	DispatchFinishAt  int    `json:"dispatch_finish_at_elapsed_seconds"`
	RetryAfterElapsed int    `json:"retry_after_elapsed_seconds"`
	Paused            bool   `json:"paused"`
	PauseReason       string `json:"pause_reason,omitempty"`
}

type kitchenQueueView struct {
	Count     int
	HeadID    string
	Remaining int
}

var (
	menuRoutes                  = []string{"RestaurantMenuDishesComplete", "RestaurantMenuDrinksComplete", "RestaurantMenuClickCard", "RestaurantMenuReadCard", "RestaurantMenuDishPageEnd", "RestaurantMenuDrinkPageEnd"}
	runtimeRoutes               = []string{"RestaurantRuntimeFailure", "RestaurantRuntimeSwitchKitchen", "RestaurantRuntimeLocateDish", "RestaurantRuntimeWaitQueue", "RestaurantRuntimeReturn", "RestaurantRuntimeIdle"}
	restaurantData              Data
	restaurantDataOnce          sync.Once
	restaurantDataErr           error
	menuSizeRequirementPatterns = []*regexp.Regexp{
		regexp.MustCompile(`(\d+)道菜品和(\d+)种饮品`),
		regexp.MustCompile(`Atleast(\d+)dish\(es\)and(\d+)drink\(s\)`),
		regexp.MustCompile(`料理：(\d+)品以上、飲み物：(\d+)種類以上`),
		regexp.MustCompile(`최소(\d+)개의요리와최소(\d+)종의음료가필요합니다\.`),
		regexp.MustCompile(`(\d+)道菜品和(\d+)種飲品`),
	}
	menuQuantityPatterns = map[string][]*regexp.Regexp{
		"dish":  {regexp.MustCompile(`可烹饪(∞|\d+)`), regexp.MustCompile(`Preparable:(∞|\d+)`), regexp.MustCompile(`調理可能：(∞|\d+)`), regexp.MustCompile(`(∞|\d+)조리가능`), regexp.MustCompile(`可烹飪(∞|\d+)`)},
		"drink": {regexp.MustCompile(`库存(∞|\d+)`), regexp.MustCompile(`Stock:(∞|\d+)`), regexp.MustCompile(`在庫(∞|\d+)`), regexp.MustCompile(`재고(∞|\d+)`), regexp.MustCompile(`庫存(∞|\d+)`)},
	}
	kitchenSecondsPattern = regexp.MustCompile(`(\d+)\s*[sS秒초]`)
)

// Run applies one decision to the current task's attach state and preset routes.
func (a *DecisionAction) Run(ctx *maa.Context, arg *maa.CustomActionArg) bool {
	if ctx == nil || arg == nil || arg.RecognitionDetail == nil {
		return false
	}
	var param decisionParam
	if err := json.Unmarshal([]byte(arg.CustomActionParam), &param); err != nil {
		return false
	}
	raw, err := ctx.GetNodeJSON("RestaurantState")
	if err != nil {
		return false
	}
	var node struct {
		Attach struct {
			State session `json:"state"`
		} `json:"attach"`
	}
	if err = json.Unmarshal([]byte(raw), &node); err != nil {
		return false
	}
	state := &node.Attach.State
	patch := make(map[string]any)
	observed := observationHits(ctx.GetTasker(), arg.RecognitionDetail)
	switch param.Step {
	case "operators":
		err = recordOperators(state, param, arg.RecognitionDetail)
	case "requirements":
		err = prepareMenu(state, arg.RecognitionDetail)
	case "menu_page":
		err = readMenuPage(state, param.Kind, arg.RecognitionDetail, patch)
	case "menu_card":
		err = chooseMenuCard(state, arg.RecognitionDetail, observed, patch)
	case "menu_confirm":
		if state.Menu.PendingID == "" {
			return false
		}
		rememberMenuItem(&state.Menu, state.Menu.PendingID)
		state.Menu.PendingID = ""
		state.Menu.Index++
		err = nextMenuCard(state, patch)
	case "runtime", "verify":
		err = decideRuntime(state, arg.RecognitionDetail, observed, param.Step == "verify", patch)
	case "dispatch":
		err = prepareDispatch(state, arg.Box, patch)
	default:
		return false
	}
	if err != nil {
		log.Error().Err(err).Str("component", "RestaurantDecisionAction").Str("step", param.Step).Msg("restaurant decision failed")
		return false
	}
	patch["RestaurantState"] = map[string]any{"attach": map[string]any{"state": state}}
	if err := ctx.OverridePipeline(patch); err != nil {
		log.Error().Err(err).Str("component", "RestaurantDecisionAction").Msg("cannot store restaurant decision")
		return false
	}
	return true
}

func loadRestaurantData() (Data, error) {
	restaurantDataOnce.Do(func() {
		restaurantDataErr = resource.ReadJsonResource("data/Restaurant/restaurant.json", &restaurantData)
		if restaurantDataErr == nil && (len(restaurantData.Dishes) == 0 || len(restaurantData.Drinks) == 0) {
			restaurantDataErr = fmt.Errorf("restaurant data is missing dishes or drinks")
		}
	})
	return restaurantData, restaurantDataErr
}

// Recognition readers unwrap the current Action input, never a previous frame.
func findDetail(detail *maa.RecognitionDetail, name string) *maa.RecognitionDetail {
	if detail == nil {
		return nil
	}
	if detail.Name == name {
		return detail
	}
	for _, child := range detail.CombinedResult {
		if result := findDetail(child, name); result != nil {
			return result
		}
	}
	return nil
}

// CombinedResult children omit Hit in maa-framework-go beta.19. Resolve their
// current recognition IDs instead of treating the zero value as a failed match.
func observationHits(tasker *maa.Tasker, detail *maa.RecognitionDetail) func(string) bool {
	cache := make(map[int64]bool)
	return func(name string) bool {
		result := findDetail(detail, name)
		if result == nil || tasker == nil {
			return false
		}
		if value, found := cache[result.ID]; found {
			return value
		}
		actual, err := tasker.GetRecognitionDetail(result.ID)
		matched := err == nil && actual != nil && actual.Hit
		cache[result.ID] = matched
		return matched
	}
}

type ocrText struct {
	Text string
	Box  maa.Rect
}

func readOCR(detail *maa.RecognitionDetail, name string) []ocrText {
	detail = findDetail(detail, name)
	if detail == nil || detail.Results == nil {
		return nil
	}
	results := detail.Results.Filtered
	if len(results) == 0 {
		results = detail.Results.All
	}
	if len(results) == 0 && detail.Results.Best != nil {
		results = []*maa.RecognitionResult{detail.Results.Best}
	}
	var texts []ocrText
	seen := make(map[string]bool)
	for _, result := range results {
		if result == nil {
			continue
		}
		value, ok := result.AsOCR()
		if !ok || value == nil || strings.TrimSpace(value.Text) == "" {
			continue
		}
		key := fmt.Sprintf("%s:%v", value.Text, value.Box)
		if !seen[key] {
			texts = append(texts, ocrText{Text: strings.TrimSpace(value.Text), Box: value.Box})
			seen[key] = true
		}
	}
	return texts
}

func readText(detail *maa.RecognitionDetail, name string) string {
	var text strings.Builder
	for _, value := range readOCR(detail, name) {
		text.WriteString(value.Text)
	}
	return text.String()
}

func readIcon(detail *maa.RecognitionDetail, name string) (string, error) {
	result := findDetail(detail, name)
	if result == nil {
		return "", fmt.Errorf("missing icon observation %s", name)
	}
	parsed, _, err := iconrecognition.ParseRecognitionDetail(result)
	if err != nil {
		return "", err
	}
	if parsed.Error != nil && parsed.Error.Code != iconrecognition.ErrorCodeNoMatch {
		return "", fmt.Errorf("%s: %s", name, parsed.Error.Message)
	}
	if !parsed.Matched || len(parsed.Matches) == 0 {
		return "", nil
	}
	return parsed.Matches[0].ItemID, nil
}

func route(patch map[string]any, routes []string, selected string) {
	for _, name := range routes {
		patch[name] = map[string]any{"enabled": name == selected}
	}
}

func iconCandidates(patch map[string]any, name string, ids []string) {
	patch[name] = map[string]any{"custom_recognition_param": iconrecognition.NewParams(
		iconrecognition.WithGridType(iconrecognition.GridTypeSingleROI),
		iconrecognition.WithItemFilters(iconrecognition.ItemFilter("Restaurant:*"), iconrecognition.StorageFilter().Normal.Usable),
		iconrecognition.WithItemIDs(ids...),
	)}
}

// Kitchen and menu decisions retain UI order; Pipeline handles selection and confirmation.
func recordOperators(state *session, param decisionParam, detail *maa.RecognitionDetail) error {
	if param.Kitchen < 1 || param.Kitchen > 3 || (param.View != "picker" && param.View != "setup") {
		return fmt.Errorf("invalid kitchen/view")
	}
	data, err := loadRestaurantData()
	if err != nil {
		return err
	}
	ids := make([]string, 0, 2)
	for index := 1; index <= 2; index++ {
		name := fmt.Sprintf("RestaurantKitchen%dOperator%dNameOCR", param.Kitchen, index)
		if param.View == "picker" {
			name = fmt.Sprintf("RestaurantKitchenPickerOperator%dNameOCR", index)
		}
		id, err := kitchenCharacterID(data.Characters, readText(detail, name))
		if err != nil {
			return err
		}
		ids = append(ids, id)
	}
	if ids[0] == ids[1] {
		return fmt.Errorf("duplicate cooks in kitchen %d", param.Kitchen)
	}
	if param.View == "setup" {
		if !slices.Equal(state.Kitchens[param.Kitchen-1].CharacterIDs, ids) {
			return fmt.Errorf("kitchen %d cooks changed during confirmation", param.Kitchen)
		}
		return nil
	}
	for index, kitchen := range state.Kitchens {
		if index != param.Kitchen-1 && (contains(kitchen.CharacterIDs, ids[0]) || contains(kitchen.CharacterIDs, ids[1])) {
			return fmt.Errorf("cook assigned to two kitchens")
		}
	}
	state.Kitchens[param.Kitchen-1] = Kitchen{CharacterIDs: ids}
	return nil
}

func prepareMenu(state *session, detail *maa.RecognitionDetail) error {
	text := strings.Join(strings.Fields(readText(detail, "RestaurantMenuRequirementOCR")), "")
	for _, pattern := range menuSizeRequirementPatterns {
		sizes := pattern.FindStringSubmatch(text)
		if len(sizes) != 3 {
			continue
		}
		dishes, _ := strconv.Atoi(sizes[1])
		drinks, _ := strconv.Atoi(sizes[2])
		if dishes <= 0 || dishes > PrestockTrayLimit || drinks < 0 {
			return fmt.Errorf("invalid menu quantities %q", text)
		}
		for index, kitchen := range state.Kitchens {
			if len(kitchen.CharacterIDs) != 2 {
				return fmt.Errorf("kitchen %d does not have two verified cooks", index+1)
			}
		}
		state.Menu = menuState{MinimumDishes: dishes, MinimumDrinks: drinks}
		return nil
	}
	return fmt.Errorf("could not read menu quantities from %q", text)
}

func readMenuPage(state *session, kind string, detail *maa.RecognitionDetail, patch map[string]any) error {
	patterns, valid := menuQuantityPatterns[kind]
	if !valid || state.Menu.PendingID != "" {
		return fmt.Errorf("invalid menu page state")
	}
	cards := make([]maa.Rect, 0, 8)
	complete := 0
	for _, text := range readOCR(detail, "RestaurantMenuAvailabilityOCR") {
		normalized := strings.Join(strings.Fields(text.Text), "")
		quantity := ""
		for _, pattern := range patterns {
			if match := pattern.FindStringSubmatch(normalized); len(match) == 2 {
				quantity = match[1]
				break
			}
		}
		if quantity == "" {
			continue
		}
		// Map the variable OCR row to a card anchor; child ROI offsets live in Pipeline.
		x := 552
		if text.Box[0] >= 860 {
			x = 870
		}
		y := text.Box[1] + text.Box[3]/2 - 78
		if y+14 < 134 || y+78 > 536 {
			continue
		}
		complete++
		count, _ := strconv.Atoi(quantity)
		if quantity == "∞" || count > 0 {
			cards = append(cards, maa.Rect{x, y, 307, 93})
		}
	}
	if complete == 0 {
		return fmt.Errorf("no complete %s menu cards were recognized", kind)
	}
	sort.Slice(cards, func(i, j int) bool {
		if cards[i][1] != cards[j][1] {
			return cards[i][1] < cards[j][1]
		}
		return cards[i][0] < cards[j][0]
	})
	for first := 0; first < len(cards); {
		last := first + 1
		for last < len(cards) && cards[last][1]-cards[first][1] < 46 {
			last++
		}
		sort.SliceStable(cards[first:last], func(i, j int) bool { return cards[first+i][0] < cards[first+j][0] })
		first = last
	}
	state.Menu.Kind, state.Menu.Cards, state.Menu.Index = kind, cards, 0
	return nextMenuCard(state, patch)
}

func rememberMenuItem(menu *menuState, id string) {
	if menu.Kind == "dish" {
		if !contains(menu.Dishes, id) {
			menu.Dishes = append(menu.Dishes, id)
		}
	} else if !contains(menu.Drinks, id) {
		menu.Drinks = append(menu.Drinks, id)
	}
}

func chooseMenuCard(state *session, detail *maa.RecognitionDetail, observed func(string) bool, patch map[string]any) error {
	menu := &state.Menu
	if menu.Index < 0 || menu.Index >= len(menu.Cards) || menu.PendingID != "" {
		return fmt.Errorf("menu candidate state is missing")
	}
	id, err := readIcon(detail, "RestaurantMenuCardIcon")
	if err != nil {
		return err
	}
	if id == "" {
		return fmt.Errorf("menu card icon is unrecognized")
	}
	if observed("RestaurantMenuCardSelected") {
		rememberMenuItem(menu, id)
		menu.Index++
		return nextMenuCard(state, patch)
	}
	if contains(menu.Dishes, id) || contains(menu.Drinks, id) {
		return fmt.Errorf("verified menu selection %q disappeared", id)
	}
	menu.PendingID = id
	iconCandidates(patch, "RestaurantMenuPendingIcon", []string{id})
	route(patch, menuRoutes, "RestaurantMenuClickCard")
	return nil
}

func nextMenuCard(state *session, patch map[string]any) error {
	menu := &state.Menu
	selected, required, suffix := len(menu.Dishes), menu.MinimumDishes, "Dish"
	if menu.Kind == "drink" {
		selected, required, suffix = len(menu.Drinks), menu.MinimumDrinks, "Drink"
	}
	if selected >= required {
		if menu.Kind == "drink" {
			data, err := loadRestaurantData()
			if err != nil {
				return err
			}
			plan, err := BuildMenuPlan(data, menu.Dishes, menu.Drinks, state.Kitchens)
			if err != nil {
				return err
			}
			ledger, err := NewLedger(plan)
			if err != nil {
				return err
			}
			state.Runtime = &runtimeState{Ledger: *ledger}
			dishes := append([]string(nil), menu.Dishes...)
			all := append(append([]string(nil), dishes...), menu.Drinks...)
			for table := 1; table <= 6; table++ {
				for _, layout := range []string{"SinglePoint1", "DoublePoint1", "DoublePoint2"} {
					iconCandidates(patch, fmt.Sprintf("RestaurantTable%d%sIcon", table, layout), all)
				}
			}
			iconCandidates(patch, "RestaurantCookingHeadIcon", dishes)
			for slot := 1; slot <= 6; slot++ {
				iconCandidates(patch, fmt.Sprintf("RestaurantRecipe%dIcon", slot), dishes)
			}
		}
		name := "RestaurantMenuDishesComplete"
		if menu.Kind == "drink" {
			name = "RestaurantMenuDrinksComplete"
		}
		route(patch, menuRoutes, name)
		return nil
	}
	if menu.Index >= len(menu.Cards) {
		route(patch, menuRoutes, "RestaurantMenu"+suffix+"PageEnd")
		return nil
	}
	data, err := loadRestaurantData()
	if err != nil {
		return err
	}
	var ids []string
	if menu.Kind == "dish" {
		for _, dish := range data.Dishes {
			ids = append(ids, dish.ID)
		}
	} else {
		for _, drink := range data.Drinks {
			ids = append(ids, drink.ID)
		}
	}
	iconCandidates(patch, "RestaurantMenuCardIcon", ids)
	patch["RestaurantMenuCardAnchor"] = map[string]any{"roi": menu.Cards[menu.Index]}
	route(patch, menuRoutes, "RestaurantMenuReadCard")
	return nil
}

// Runtime observations arrive as one Pipeline graph, so every fact belongs to the same screenshot.
func readRuntimeFrame(detail *maa.RecognitionDetail, observed func(string) bool, state *runtimeState, full bool) (GameSnapshot, int, kitchenQueueView, error) {
	remaining := readText(detail, "RestaurantRuntimeClockOCR")
	if _, err := ParseCountdown(remaining); err != nil {
		return GameSnapshot{}, 0, kitchenQueueView{}, err
	}
	kitchen := 0
	for k := 1; k <= 3; k++ {
		if observed(fmt.Sprintf("RestaurantKitchen%dView", k)) {
			kitchen = k
			break
		}
	}
	if kitchen == 0 {
		return GameSnapshot{}, 0, kitchenQueueView{}, fmt.Errorf("selected kitchen or queue is uncertain")
	}
	view := kitchenQueueView{Count: -1}
	for n := 0; n <= 3; n++ {
		if observed(fmt.Sprintf("RestaurantKitchen%dCount%d", kitchen, n)) {
			view.Count = n
			break
		}
	}
	if view.Count < 0 {
		return GameSnapshot{}, 0, view, fmt.Errorf("kitchen queue dots are uncertain")
	}
	if view.Count > 0 {
		id, err := readIcon(detail, "RestaurantCookingHeadIcon")
		if err != nil {
			return GameSnapshot{}, 0, view, err
		}
		view.HeadID = id
		seconds := kitchenSecondsPattern.FindStringSubmatch(readText(detail, "RestaurantCookingHeadSecondsOCR"))
		if id == "" || len(seconds) != 2 {
			return GameSnapshot{}, 0, view, fmt.Errorf("cooking head or countdown is unrecognized")
		}
		view.Remaining, _ = strconv.Atoi(seconds[1])
	}
	if !full {
		return dispatchQueueSnapshot(&state.Ledger, remaining), kitchen, view, nil
	}
	snapshot := GameSnapshot{Remaining: remaining, Known: true, Complete: true}
	var firstErr error
	for table := 1; table <= 6; table++ {
		points, err := readTablePoints(detail, observed, table, state)
		if err != nil {
			snapshot.Complete = false
			if firstErr == nil {
				firstErr = err
			}
			points = make([]OrderPoint, 2)
			for point := 1; point <= 2; point++ {
				previous, found := state.Ledger.Points[orderPointStorageKey(OrderPointKey{Table: table, Point: point})]
				snapshot.Known = snapshot.Known && found
				points[point-1] = previous
			}
		}
		snapshot.Points = append(snapshot.Points, points...)
	}
	if snapshot.Complete {
		for slot := 1; slot <= 6; slot++ {
			prefix := fmt.Sprintf("RestaurantRecipe%d", slot)
			if !observed(prefix + "RestockMarker") {
				continue
			}
			id, err := readIcon(detail, prefix+"Icon")
			if err != nil || id == "" {
				snapshot.Complete = false
				firstErr = fmt.Errorf("supplement marker has an unrecognized dish: %v", err)
				break
			}
			snapshot.NeedsRestock = append(snapshot.NeedsRestock, id)
		}
	}
	return snapshot, kitchen, view, firstErr
}

func readTablePoints(detail *maa.RecognitionDetail, observed func(string) bool, table int, state *runtimeState) ([]OrderPoint, error) {
	points := []OrderPoint{{Key: OrderPointKey{Table: table, Point: 1}}, {Key: OrderPointKey{Table: table, Point: 2}}}
	prefix := fmt.Sprintf("RestaurantTable%d", table)
	layout, count := "Single", 1
	if observed(prefix + "DoubleOrder") {
		layout, count = "Double", 2
	} else if !observed(prefix + "SingleOrder") {
		if !observed(prefix + "Inactive") {
			return nil, fmt.Errorf("table %d layout or item is uncertain", table)
		}
		unserved := false
		for _, point := range points {
			previous := state.Ledger.Points[orderPointStorageKey(point.Key)]
			if _, dish := state.Ledger.MenuByID[previous.ItemID]; dish && previous.Exists && !previous.Served {
				unserved = true
			}
		}
		if unserved {
			if !observed(prefix + "Dining") {
				return nil, fmt.Errorf("table %d is waiting for delivery or eating confirmation", table)
			}
			for index, point := range points {
				if previous := state.Ledger.Points[orderPointStorageKey(point.Key)]; previous.Exists {
					previous.Served = true
					points[index] = previous
				}
			}
		}
		return points, nil
	}
	for point := 1; point <= count; point++ {
		name := fmt.Sprintf("%s%sPoint%d", prefix, layout, point)
		id, err := readIcon(detail, name+"Icon")
		if err != nil {
			return nil, err
		}
		if id == "" {
			return nil, fmt.Errorf("table %d point %d item is unrecognized", table, point)
		}
		points[point-1].ItemID = id
		points[point-1].Exists = true
		points[point-1].Served = observed(name + "Served")
	}
	return points, nil
}

func decideRuntime(state *session, detail *maa.RecognitionDetail, observed func(string) bool, verify bool, patch map[string]any) error {
	current := state.Runtime
	if current == nil {
		return fmt.Errorf("runtime ledger is missing")
	}
	if verify && current.Ledger.Pending == nil {
		route(patch, runtimeRoutes, "RestaurantRuntimeReturn")
		return nil
	}
	full := observed("RestaurantRuntimeFullFrame")
	snapshot, kitchen, view, err := readRuntimeFrame(detail, observed, current, full)
	added := false
	if err == nil || snapshot.Known {
		snapshotErr := err
		added, err = synchronizeRuntimeFrame(current, snapshot, kitchen, view, verify)
		if err == nil {
			err = snapshotErr
		}
	}
	if err != nil {
		if current.PauseReason != err.Error() {
			log.Warn().Err(err).Msg("restaurant is waiting for consistent observations")
		}
		current.PauseReason = err.Error()
		name := "RestaurantRuntimeIdle"
		if verify {
			name = "RestaurantRuntimeWaitQueue"
			if current.Ledger.Pending == nil {
				name = "RestaurantRuntimeReturn"
			}
			patch["RestaurantRuntimeVerifyDispatch"] = map[string]any{"all_of": []string{"RestaurantRuntimeFullFrame"}}
		}
		if current.Paused {
			name = "RestaurantRuntimeFailure"
		}
		route(patch, runtimeRoutes, name)
		return nil
	}
	current.PauseReason = ""
	if verify {
		if added {
			route(patch, runtimeRoutes, "RestaurantRuntimeReturn")
			return nil
		}
		if current.Ledger.Clock.ElapsedSeconds < current.RetryAfterElapsed {
			route(patch, runtimeRoutes, "RestaurantRuntimeWaitQueue")
			return nil
		}
	}
	pending := current.Ledger.Pending
	if pending == nil {
		route(patch, runtimeRoutes, "RestaurantRuntimeIdle")
		return nil
	}
	if current.Ledger.counterCount() >= CounterTrayCount || len(current.Ledger.Kitchens[pending.Kitchen-1]) >= KitchenQueueSize {
		name := "RestaurantRuntimeIdle"
		if verify {
			name = "RestaurantRuntimeWaitQueue"
		}
		route(patch, runtimeRoutes, name)
		return nil
	}
	patch["RestaurantRuntimeTargetKitchen"] = map[string]any{"all_of": []string{fmt.Sprintf("RestaurantKitchen%dSelected", pending.Kitchen)}}
	for slot := 1; slot <= 6; slot++ {
		iconCandidates(patch, fmt.Sprintf("RestaurantRecipe%dTargetIcon", slot), []string{pending.DishID})
	}
	if kitchen != pending.Kitchen {
		route(patch, runtimeRoutes, "RestaurantRuntimeSwitchKitchen")
		patch["RestaurantRuntimeSwitchKitchen"].(map[string]any)["target"] = fmt.Sprintf("RestaurantKitchen%dTab", pending.Kitchen)
	} else {
		route(patch, runtimeRoutes, "RestaurantRuntimeLocateDish")
	}
	return nil
}

func prepareDispatch(state *session, box maa.Rect, patch map[string]any) error {
	current := state.Runtime
	if current == nil || current.Ledger.Pending == nil || box[2] <= 0 || box[3] <= 0 {
		return fmt.Errorf("pending dispatch or recipe observation is missing")
	}
	pending := current.Ledger.Pending
	elapsed := current.Ledger.Clock.ElapsedSeconds
	start := elapsed
	queue := current.Ledger.Kitchens[pending.Kitchen-1]
	if len(queue) > 0 {
		start = max(start, queue[len(queue)-1].FinishAt)
	}
	current.DispatchFinishAt = start + pending.Duration
	current.RetryAfterElapsed = elapsed + 1
	iconCandidates(patch, "RestaurantRuntimeClickDishIcon", []string{pending.DishID})
	patch["RestaurantRuntimeClickDishIcon"].(map[string]any)["roi"] = box
	portions := current.Ledger.counterCount() + 1
	for _, queue := range current.Ledger.Kitchens {
		portions += len(queue)
	}
	frame := "RestaurantRuntimeFullFrame"
	if current.PauseReason == "" && portions < CounterTrayCount {
		frame = "RestaurantRuntimeDispatchFrame"
	}
	patch["RestaurantRuntimeVerifyDispatch"] = map[string]any{"all_of": []string{frame}}
	return nil
}

func dispatchQueueSnapshot(ledger *Ledger, remaining string) GameSnapshot {
	points := make([]OrderPoint, 0, len(ledger.Points))
	for _, point := range ledger.Points {
		points = append(points, point)
	}
	return GameSnapshot{Remaining: remaining, Known: true, Complete: true, Points: points}
}

// FIFO reconciliation and stock decisions. UI facts are supplied by Pipeline.
func synchronizeRuntimeFrame(state *runtimeState, snapshot GameSnapshot, kitchen int, view kitchenQueueView, verify bool) (bool, error) {
	ledger := &state.Ledger
	clock := ledger.Clock
	elapsed, err := clock.Observe(snapshot.Remaining)
	if err != nil {
		return false, err
	}
	queue := ledger.Kitchens[kitchen-1]
	removed, added, err := queueProgress(queue, view, ledger.Pending, elapsed, verify)
	if err != nil {
		return false, err
	}
	if verify && !added && state.DispatchFinishAt > 0 && elapsed >= state.DispatchFinishAt {
		state.Paused = snapshot.Complete
		return false, fmt.Errorf("dispatch result was not observed before the dish could have finished; refusing to duplicate it")
	}
	if len(queue) > removed && (view.HeadID == "" || view.HeadID != queue[removed].DishID) {
		return false, fmt.Errorf("visible cooking head does not match kitchen %d FIFO", kitchen)
	}
	for index := 0; index < removed; index++ {
		queue[index].FinishAt = min(queue[index].FinishAt, elapsed)
	}
	calibrateQueue(queue[removed:], elapsed, view)
	if ledger.counterCount() >= CounterTrayCount {
		// advanceCooking will apply this already-observed full-counter interval
		// to every kitchen. Compensate this visible queue before that advance.
		for index := range queue {
			queue[index].FinishAt -= elapsed - ledger.AdvancedAt
		}
	}
	observation := ledger.Observe(snapshot)
	if observation.Paused {
		state.Paused = snapshot.Complete
		return false, fmt.Errorf("ledger cannot be reconciled: %s", observation.Reason)
	}
	if added {
		dispatch := *ledger.Pending
		if err := ledger.ConfirmDispatch(dispatch); err != nil {
			state.Paused = snapshot.Complete
			return false, err
		}
		state.DispatchFinishAt = 0
		state.RetryAfterElapsed = 0
	}
	// Another kitchen can have filled the counter during the same interval.
	// Its simulated pause is already included in the visible head countdown.
	calibrateQueue(ledger.Kitchens[kitchen-1], elapsed, view)
	expected := queueIDs(ledger.Kitchens[kitchen-1])
	if len(expected) != view.Count || (len(expected) > 0 && expected[0] != view.HeadID) {
		state.Paused = snapshot.Complete
		return false, fmt.Errorf("kitchen %d queue disagrees with the ledger: visible count=%d head=%q ledger=%v", kitchen, view.Count, view.HeadID, expected)
	}
	return added, nil
}

// Match only removal of a known FIFO prefix and, during verification, exactly
// one pending dish. An identical dish completing during the click can make
// both interpretations possible; wait until the game clock resolves it.
func queueProgress(queue []CookingJob, view kitchenQueueView, pending *Dispatch, elapsed int, verify bool) (int, bool, error) {
	matches := func(ids []string) bool {
		return len(ids) == view.Count && (len(ids) == 0 || ids[0] == view.HeadID)
	}
	without, with := -1, -1
	for removed := 0; removed <= len(queue); removed++ {
		expected := queueIDs(queue[removed:])
		if matches(expected) {
			without = removed
		}
		if verify && pending != nil && matches(append(expected, pending.DishID)) {
			with = removed
		}
	}
	if with >= 0 && without >= 0 {
		if !slices.Equal(queueIDs(queue[without:]), append(queueIDs(queue[with:]), pending.DishID)) {
			return 0, false, fmt.Errorf("queue count and head cannot distinguish the pending dish from a completed prefix")
		}
		if with > 0 && (view.Remaining == 0 || queue[with-1].FinishAt > elapsed) {
			return 0, false, fmt.Errorf("queue change is ambiguous while a dish completes")
		}
		return with, true, nil
	}
	if with >= 0 {
		return with, true, nil
	}
	if without >= 0 {
		return without, false, nil
	}
	return 0, false, fmt.Errorf("visible queue count=%d head=%q does not match the recorded FIFO", view.Count, view.HeadID)
}

func calibrateQueue(queue []CookingJob, elapsed int, view kitchenQueueView) {
	if len(queue) == 0 || queue[0].DishID != view.HeadID {
		return
	}
	// A visible head still occupies its slot at 0s. Keep it beyond this frame
	// until the next queue observation proves that it has left.
	queue[0].FinishAt = elapsed + max(1, view.Remaining)
	for index := 1; index < len(queue); index++ {
		queue[index].FinishAt = queue[index-1].FinishAt + queue[index].Duration
	}
}

func queueIDs(queue []CookingJob) []string {
	ids := make([]string, 0, len(queue))
	for _, job := range queue {
		ids = append(ids, job.DishID)
	}
	return ids
}

func kitchenCharacterID(characters []Character, text string) (string, error) {
	name := strings.Join(strings.Fields(text), "")
	var selected *Character
	for index := range characters {
		character := &characters[index]
		matched := false
		for _, localizedName := range character.Names {
			localizedName = strings.Join(strings.Fields(localizedName), "")
			if localizedName != "" && strings.HasSuffix(name, localizedName) {
				matched = true
				break
			}
		}
		if !matched {
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

// Cooking data and kitchen-local modifiers.
type Dish struct {
	ID       string   `json:"id"`
	Category string   `json:"category"`
	Time     int      `json:"time_seconds"`
	Affixes  []string `json:"affixes"`
}

type Drink struct {
	ID string `json:"id"`
}

type Skill struct {
	ID         string  `json:"id"`
	Effect     string  `json:"effect"`
	Category   string  `json:"category"`
	Affix      string  `json:"affix"`
	SpeedBonus float64 `json:"speed_bonus"`
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
	Dishes     []Dish      `json:"dishes"`
	Drinks     []Drink     `json:"drinks"`
	Characters []Character `json:"characters"`
	Skills     []Skill     `json:"skills"`
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

// Order, stock, queue, and game-clock ledger.
const (
	KitchenCount      = 3
	KitchenQueueSize  = 3
	CounterTrayCount  = 8
	PrestockTrayLimit = 6
)

var (
	closingCountdownPattern = regexp.MustCompile(`^[^0-9:]*营业[^0-9:]*([0-9]{1,2})[^0-9:]*$`)
	closingCountdownPhrases = []string{"营业", "Session Ending Soon", "営業終了間近", "곧 운영이 종료됩니다.", "即將結束營業"}
	countdownNumberPattern  = regexp.MustCompile(`[0-9]{1,2}`)
)

type OrderPointKey struct {
	Table int `json:"table"`
	Point int `json:"point"`
}

type OrderPoint struct {
	Key    OrderPointKey `json:"key"`
	ItemID string        `json:"item_id"`
	Exists bool          `json:"exists"`
	Served bool          `json:"served"`
}

type GameSnapshot struct {
	Remaining    string       `json:"remaining"`
	Known        bool         `json:"known"`
	Complete     bool         `json:"complete"`
	Points       []OrderPoint `json:"points"`
	NeedsRestock []string     `json:"needs_restock"`
}

type CookingJob struct {
	DishID   string `json:"dish_id"`
	Duration int    `json:"duration_seconds"`
	FinishAt int    `json:"finish_at_elapsed_seconds"`
}

type Dispatch struct {
	DishID   string `json:"dish_id"`
	Kitchen  int    `json:"kitchen"`
	Duration int    `json:"duration_seconds"`
	FinishAt int    `json:"finish_at_elapsed_seconds"`
	Prestock bool   `json:"prestock"`
}

type Ledger struct {
	Clock      GameClock
	Kitchens   [KitchenCount][]CookingJob
	Counter    map[string]int
	Plan       MenuPlan
	MenuByID   map[string]DishSelection
	DrinkIDs   map[string]struct{}
	Points     map[string]OrderPoint
	AdvancedAt int
	Pending    *Dispatch
}

type Observation struct {
	ElapsedSeconds int
	Paused         bool
	Reason         string
	Consumed       []string
	Completions    []string
	Dispatch       *Dispatch
}

type GameClock struct {
	Initialized      bool `json:"initialized"`
	InitialRemaining int  `json:"initial_remaining_seconds"`
	LastRemaining    int  `json:"last_remaining_seconds"`
	ElapsedSeconds   int  `json:"elapsed_seconds"`
}

func (c *GameClock) Observe(text string) (int, error) {
	remaining, err := ParseCountdown(text)
	if err != nil {
		return c.ElapsedSeconds, err
	}
	if !c.Initialized {
		c.Initialized = true
		c.InitialRemaining = remaining
		c.LastRemaining = remaining
		return c.ElapsedSeconds, nil
	}
	if remaining > c.LastRemaining {
		return c.ElapsedSeconds, fmt.Errorf("game countdown moved backwards from %d to %d", c.LastRemaining, remaining)
	}
	c.ElapsedSeconds += c.LastRemaining - remaining
	c.LastRemaining = remaining
	return c.ElapsedSeconds, nil
}

func ParseCountdown(text string) (int, error) {
	text = strings.TrimSpace(text)
	parts := strings.Split(text, ":")
	if len(parts) != 2 {
		closingText := strings.ToLower(strings.Join(strings.Fields(text), ""))
		for _, phrase := range closingCountdownPhrases {
			localizedPhrase := strings.ToLower(strings.Join(strings.Fields(phrase), ""))
			if !strings.Contains(closingText, localizedPhrase) {
				continue
			}
			matches := countdownNumberPattern.FindAllString(text, -1)
			if len(matches) > 0 {
				seconds, _ := strconv.Atoi(matches[len(matches)-1])
				if seconds <= 20 {
					return seconds, nil
				}
			}
			break
		}
		if match := closingCountdownPattern.FindStringSubmatch(text); len(match) == 2 {
			seconds, _ := strconv.Atoi(match[1])
			if seconds <= 20 {
				return seconds, nil
			}
		}
		return 0, fmt.Errorf("invalid countdown %q", text)
	}
	minutes, err := strconv.Atoi(parts[0])
	if err != nil || minutes < 0 {
		return 0, fmt.Errorf("invalid countdown minutes %q", parts[0])
	}
	seconds, err := strconv.Atoi(parts[1])
	if err != nil || seconds < 0 || seconds >= 60 {
		return 0, fmt.Errorf("invalid countdown seconds %q", parts[1])
	}
	return minutes*60 + seconds, nil
}

func NewLedger(plan MenuPlan) (*Ledger, error) {
	if len(plan.Dishes) == 0 || len(plan.Dishes) > PrestockTrayLimit {
		return nil, fmt.Errorf("dish menu size %d is outside 1..%d", len(plan.Dishes), PrestockTrayLimit)
	}
	ledger := &Ledger{
		Counter:  make(map[string]int),
		Plan:     plan,
		MenuByID: make(map[string]DishSelection, len(plan.Dishes)),
		DrinkIDs: make(map[string]struct{}, len(plan.Drinks)),
		Points:   make(map[string]OrderPoint),
	}
	for _, dish := range plan.Dishes {
		if dish.DishID == "" {
			return nil, fmt.Errorf("menu contains an empty dish ID")
		}
		for _, duration := range dish.KitchenTimes {
			if duration <= 0 {
				return nil, fmt.Errorf("dish %q has non-positive kitchen duration", dish.DishID)
			}
		}
		if _, exists := ledger.MenuByID[dish.DishID]; exists {
			return nil, fmt.Errorf("menu contains duplicate dish %q", dish.DishID)
		}
		ledger.MenuByID[dish.DishID] = dish
	}
	for _, drinkID := range plan.Drinks {
		if drinkID == "" {
			return nil, fmt.Errorf("menu contains an empty drink ID")
		}
		ledger.DrinkIDs[drinkID] = struct{}{}
	}
	return ledger, nil
}

func (l *Ledger) Observe(snapshot GameSnapshot) Observation {
	if !snapshot.Known || len(snapshot.Points) != 12 {
		return Observation{ElapsedSeconds: l.Clock.ElapsedSeconds, Paused: true, Reason: "table or menu state is uncertain"}
	}
	current, err := validatePoints(snapshot.Points, l.MenuByID, l.DrinkIDs)
	if err != nil {
		return Observation{ElapsedSeconds: l.Clock.ElapsedSeconds, Paused: true, Reason: err.Error()}
	}
	for _, dishID := range snapshot.NeedsRestock {
		if _, selected := l.MenuByID[dishID]; !selected {
			return Observation{ElapsedSeconds: l.Clock.ElapsedSeconds, Paused: true, Reason: fmt.Sprintf("supplement marker belongs to unselected dish %q", dishID)}
		}
	}
	elapsed, err := l.Clock.Observe(snapshot.Remaining)
	if err != nil {
		return Observation{ElapsedSeconds: l.Clock.ElapsedSeconds, Paused: true, Reason: err.Error()}
	}
	observation := Observation{ElapsedSeconds: elapsed}
	observation.Completions = l.advanceCooking()

	consumed := make(map[string]int)
	for key, previous := range l.Points {
		next, exists := current[key]
		if !exists || !next.Exists || next.ItemID != previous.ItemID {
			if previous.Exists && !previous.Served {
				if _, isDrink := l.DrinkIDs[previous.ItemID]; !isDrink {
					return Observation{ElapsedSeconds: elapsed, Paused: true, Reason: fmt.Sprintf("dish point %d/%d changed before its served marker was observed", previous.Key.Table, previous.Key.Point)}
				}
			}
			continue
		}
	}
	for key, next := range current {
		previous := l.Points[key]
		if !next.Exists || !next.Served || (previous.Exists && previous.ItemID == next.ItemID && previous.Served) {
			continue
		}
		if _, isDrink := l.DrinkIDs[next.ItemID]; !isDrink {
			consumed[next.ItemID]++
		}
	}
	for dishID, quantity := range consumed {
		if l.Counter[dishID] < quantity {
			return Observation{ElapsedSeconds: elapsed, Paused: true, Reason: fmt.Sprintf("served dish %q needs %d portions but counter ledger holds %d", dishID, quantity, l.Counter[dishID])}
		}
	}
	for dishID, quantity := range consumed {
		l.Counter[dishID] -= quantity
		for range quantity {
			observation.Consumed = append(observation.Consumed, dishID)
		}
	}
	l.Points = current
	observation.Completions = append(observation.Completions, l.advanceCooking()...)

	// Keep confirmed serving and clock updates, but wait for every table before dispatching.
	if !snapshot.Complete {
		return observation
	}
	if l.Pending != nil {
		pending := *l.Pending
		observation.Dispatch = &pending
		return observation
	}
	if dispatch := l.suggestDispatch(snapshot.NeedsRestock); dispatch != nil {
		l.Pending = dispatch
		copy := *dispatch
		observation.Dispatch = &copy
	}
	return observation
}

func validatePoints(points []OrderPoint, dishes map[string]DishSelection, drinks map[string]struct{}) (map[string]OrderPoint, error) {
	current := make(map[string]OrderPoint, len(points))
	for _, point := range points {
		if point.Key.Table < 1 || point.Key.Table > 6 || point.Key.Point < 1 || point.Key.Point > 2 {
			return nil, fmt.Errorf("invalid table point %d/%d", point.Key.Table, point.Key.Point)
		}
		if point.Exists {
			if point.ItemID == "" {
				return nil, fmt.Errorf("occupied table point %d/%d has no recognized item", point.Key.Table, point.Key.Point)
			}
			if _, dish := dishes[point.ItemID]; !dish {
				if _, drink := drinks[point.ItemID]; !drink {
					return nil, fmt.Errorf("table point %d/%d has unselected item %q", point.Key.Table, point.Key.Point, point.ItemID)
				}
			}
		}
		key := orderPointStorageKey(point.Key)
		if _, exists := current[key]; exists {
			return nil, fmt.Errorf("duplicate table point %d/%d", point.Key.Table, point.Key.Point)
		}
		current[key] = point
	}
	if len(current) != 12 {
		return nil, fmt.Errorf("recognized %d table points, expected 12", len(current))
	}
	return current, nil
}

func orderPointStorageKey(key OrderPointKey) string {
	return fmt.Sprintf("%d/%d", key.Table, key.Point)
}

func (l *Ledger) suggestDispatch(needsRestock []string) *Dispatch {
	if l.counterCount() >= CounterTrayCount {
		return nil
	}
	marked := make(map[string]struct{}, len(needsRestock))
	for _, dishID := range needsRestock {
		marked[dishID] = struct{}{}
	}
	for _, dish := range l.Plan.Dishes {
		if _, needs := marked[dish.DishID]; !needs {
			continue
		}
		if l.Counter[dish.DishID]+l.inFlight(dish.DishID) >= max(1, l.unservedDemand(dish.DishID)) {
			continue
		}
		if dispatch := l.makeDispatch(dish.DishID, false); dispatch != nil {
			return dispatch
		}
		return nil
	}
	for _, dish := range l.Plan.Dishes {
		if l.Counter[dish.DishID]+l.inFlight(dish.DishID) >= l.unservedDemand(dish.DishID) {
			continue
		}
		if dispatch := l.makeDispatch(dish.DishID, false); dispatch != nil {
			return dispatch
		}
		return nil
	}
	for _, dish := range l.Plan.Dishes {
		dishID := dish.DishID
		if l.Counter[dishID]+l.inFlight(dishID) > 0 {
			continue
		}
		if dispatch := l.makeDispatch(dishID, true); dispatch != nil {
			return dispatch
		}
		return nil
	}
	return nil
}

func (l *Ledger) makeDispatch(dishID string, prestock bool) *Dispatch {
	dish, exists := l.MenuByID[dishID]
	if !exists || l.counterCount() >= CounterTrayCount {
		return nil
	}
	selected := -1
	selectedFinish := 0
	selectedQueue := 0
	for index, queue := range l.Kitchens {
		if len(queue) >= KitchenQueueSize {
			continue
		}
		start := l.Clock.ElapsedSeconds
		if len(queue) > 0 && queue[len(queue)-1].FinishAt > start {
			start = queue[len(queue)-1].FinishAt
		}
		finish := start + dish.KitchenTimes[index]
		if selected < 0 || finish < selectedFinish || (finish == selectedFinish && (len(queue) < selectedQueue || (len(queue) == selectedQueue && index < selected))) {
			selected = index
			selectedFinish = finish
			selectedQueue = len(queue)
		}
	}
	if selected < 0 {
		return nil
	}
	return &Dispatch{
		DishID:   dishID,
		Kitchen:  selected + 1,
		Duration: dish.KitchenTimes[selected],
		FinishAt: selectedFinish,
		Prestock: prestock,
	}
}

func (l *Ledger) ConfirmDispatch(dispatch Dispatch) error {
	if l.Pending == nil || l.Pending.DishID != dispatch.DishID || l.Pending.Kitchen != dispatch.Kitchen || l.Pending.Prestock != dispatch.Prestock {
		return fmt.Errorf("confirmed dispatch does not match pending job")
	}
	if dispatch.Kitchen < 1 || dispatch.Kitchen > KitchenCount {
		return fmt.Errorf("invalid kitchen %d", dispatch.Kitchen)
	}
	kitchen := dispatch.Kitchen - 1
	if len(l.Kitchens[kitchen]) >= KitchenQueueSize {
		return fmt.Errorf("kitchen %d queue is full", dispatch.Kitchen)
	}
	dish := l.MenuByID[dispatch.DishID]
	start := l.Clock.ElapsedSeconds
	queue := l.Kitchens[kitchen]
	if len(queue) > 0 && queue[len(queue)-1].FinishAt > start {
		start = queue[len(queue)-1].FinishAt
	}
	duration := dish.KitchenTimes[kitchen]
	l.Kitchens[kitchen] = append(queue, CookingJob{
		DishID:   dispatch.DishID,
		Duration: duration,
		FinishAt: start + duration,
	})
	l.Pending = nil
	return nil
}

func (l *Ledger) advanceCooking() []string {
	completed := make([]string, 0)
	blockedAt := l.AdvancedAt
	for l.counterCount() < CounterTrayCount {
		next := -1
		for kitchen, queue := range l.Kitchens {
			if len(queue) > 0 && (next < 0 || queue[0].FinishAt < l.Kitchens[next][0].FinishAt) {
				next = kitchen
			}
		}
		if next < 0 || l.Kitchens[next][0].FinishAt > l.Clock.ElapsedSeconds {
			break
		}
		job := l.Kitchens[next][0]
		blockedAt = max(blockedAt, job.FinishAt)
		l.Counter[job.DishID]++
		completed = append(completed, job.DishID)
		l.Kitchens[next] = l.Kitchens[next][1:]
	}
	// All kitchens stop while the counter is full. Keep the remaining cooking
	// time instead of completing queued dishes during the blocked interval.
	if l.counterCount() >= CounterTrayCount {
		for kitchen := range l.Kitchens {
			for index := range l.Kitchens[kitchen] {
				l.Kitchens[kitchen][index].FinishAt += l.Clock.ElapsedSeconds - blockedAt
			}
		}
	}
	l.AdvancedAt = l.Clock.ElapsedSeconds
	return completed
}

func (l *Ledger) counterCount() int {
	count := 0
	for _, quantity := range l.Counter {
		count += quantity
	}
	return count
}

func (l *Ledger) inFlight(dishID string) int {
	count := 0
	for _, queue := range l.Kitchens {
		for _, job := range queue {
			if job.DishID == dishID {
				count++
			}
		}
	}
	return count
}

func (l *Ledger) unservedDemand(dishID string) int {
	count := 0
	for _, point := range l.Points {
		if point.Exists && !point.Served && point.ItemID == dishID {
			count++
		}
	}
	return count
}
