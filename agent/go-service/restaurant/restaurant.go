package restaurant

import (
	"encoding/json"
	"errors"
	"fmt"
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
	Step string `json:"step"`
	Kind string `json:"kind"`
}

type session struct {
	Menu    menuState     `json:"menu"`
	Runtime *runtimeState `json:"runtime,omitempty"`
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
	Ledger      Ledger `json:"ledger"`
	PauseReason string `json:"pause_reason,omitempty"`
}

type kitchenQueueView struct {
	Selected int
	Counts   [KitchenCount]int
}

var (
	errAmbiguousQueue           = errors.New("queue icons cannot distinguish the pending dish from a completed identical dish")
	menuRoutes                  = []string{"RestaurantMenuDishesComplete", "RestaurantMenuDrinksComplete", "RestaurantMenuClickCard", "RestaurantMenuReadCard", "RestaurantMenuDishPageEnd", "RestaurantMenuDrinkPageEnd"}
	runtimeRoutes               = []string{"RestaurantRuntimeSwitchKitchen1", "RestaurantRuntimeSwitchKitchen2", "RestaurantRuntimeSwitchKitchen3", "RestaurantRuntimeLocateDish", "RestaurantRuntimeClickDish1", "RestaurantRuntimeClickDish2", "RestaurantRuntimeClickDish3", "RestaurantRuntimeClickDish4", "RestaurantRuntimeClickDish5", "RestaurantRuntimeClickDish6", "RestaurantRuntimeWaitQueue", "RestaurantRuntimeReturn", "RestaurantRuntimeIdle"}
	restaurantData              menuCatalog
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
		err = prepareDispatch(state, arg.RecognitionDetail, observed, arg.Box, patch)
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

func loadRestaurantData() (menuCatalog, error) {
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
	var texts []ocrText
	seen := make(map[string]bool)
	for _, result := range detail.Results.Filtered {
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

// Menu decisions preserve UI order; Pipeline selects and confirms each card.
func prepareMenu(state *session, detail *maa.RecognitionDetail) error {
	text := strings.Join(strings.Fields(readText(detail, "RestaurantMenuRequirementOCR")), "")
	for _, pattern := range menuSizeRequirementPatterns {
		sizes := pattern.FindStringSubmatch(text)
		if len(sizes) != 3 {
			continue
		}
		dishes, _ := strconv.Atoi(sizes[1])
		drinks, _ := strconv.Atoi(sizes[2])
		if dishes <= 0 || dishes > PrestockTrayLimit {
			return fmt.Errorf("invalid menu quantities %q", text)
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
		if !slices.Contains(menu.Dishes, id) {
			menu.Dishes = append(menu.Dishes, id)
		}
	} else if !slices.Contains(menu.Drinks, id) {
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
	if slices.Contains(menu.Dishes, id) || slices.Contains(menu.Drinks, id) {
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
			state.Runtime = &runtimeState{Ledger: newLedger(menu.Dishes, menu.Drinks)}
			dishes := append([]string(nil), menu.Dishes...)
			all := append(append([]string(nil), dishes...), menu.Drinks...)
			for table := 1; table <= 6; table++ {
				for _, layout := range []string{"SinglePoint1", "DoublePoint1", "DoublePoint2"} {
					iconCandidates(patch, fmt.Sprintf("RestaurantTable%d%sIcon", table, layout), all)
				}
			}
			for point := 1; point < KitchenQueueSize; point++ {
				iconCandidates(patch, fmt.Sprintf("RestaurantQueuePoint%dIcon", point), dishes)
			}
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
		ids = data.Dishes
	} else {
		ids = data.Drinks
	}
	iconCandidates(patch, "RestaurantMenuCardIcon", ids)
	patch["RestaurantMenuCardAnchor"] = map[string]any{"roi": menu.Cards[menu.Index]}
	route(patch, menuRoutes, "RestaurantMenuReadCard")
	return nil
}

// Runtime facts are read by Pipeline from one screenshot, including all three queues.
func readKitchenView(observed func(string) bool) (kitchenQueueView, error) {
	view := kitchenQueueView{Counts: [KitchenCount]int{-1, -1, -1}}
	for kitchen := 1; kitchen <= KitchenCount; kitchen++ {
		if observed(fmt.Sprintf("RestaurantKitchen%dSelected", kitchen)) {
			view.Selected = kitchen
		}
		for count := 0; count <= KitchenQueueSize; count++ {
			if observed(fmt.Sprintf("RestaurantKitchen%dCount%d", kitchen, count)) {
				view.Counts[kitchen-1] = count
				break
			}
		}
		if view.Counts[kitchen-1] < 0 {
			return view, fmt.Errorf("kitchen %d queue dots are uncertain", kitchen)
		}
	}
	if view.Selected == 0 {
		return view, fmt.Errorf("selected kitchen is uncertain")
	}
	return view, nil
}

func readRuntimeFrame(detail *maa.RecognitionDetail, observed func(string) bool, state *runtimeState) (GameSnapshot, error) {
	var snapshot GameSnapshot
	for table := 1; table <= 6; table++ {
		points, err := readTablePoints(detail, observed, table, state)
		if err != nil {
			return GameSnapshot{}, err
		}
		snapshot.Points = append(snapshot.Points, points...)
	}
	for slot := 1; slot <= 6; slot++ {
		prefix := fmt.Sprintf("RestaurantRecipe%d", slot)
		if !observed(prefix + "RestockMarker") {
			continue
		}
		id, err := readIcon(detail, prefix+"Icon")
		if err != nil || id == "" {
			return GameSnapshot{}, fmt.Errorf("supplement marker has an unrecognized dish: %v", err)
		}
		snapshot.NeedsRestock = append(snapshot.NeedsRestock, id)
	}
	return snapshot, nil
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
			if _, dish := state.Ledger.DishIDs[previous.ItemID]; dish && previous.Exists && !previous.Served {
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
	ledger := &current.Ledger
	view, err := readKitchenView(observed)
	added := false
	if err == nil {
		added, err = ledger.observeQueues(view, detail, verify)
	}
	if observed("RestaurantRuntimeFullFrame") {
		snapshot, frameErr := readRuntimeFrame(detail, observed, current)
		if frameErr == nil {
			frameErr = ledger.observeTables(snapshot)
		}
		if err == nil {
			err = frameErr
		}
		if err == nil && !verify {
			ledger.Pending = ledger.suggestDispatch(snapshot.NeedsRestock)
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
			if ledger.Pending == nil {
				name = "RestaurantRuntimeReturn"
			}
			patch["RestaurantRuntimeVerifyDispatch"] = map[string]any{"all_of": []string{"RestaurantRuntimeFullFrame", "RestaurantRuntimeQueueIcons"}}
		}
		route(patch, runtimeRoutes, name)
		return nil
	}
	current.PauseReason = ""
	if verify && added {
		route(patch, runtimeRoutes, "RestaurantRuntimeReturn")
		return nil
	}
	pending := ledger.Pending
	if pending == nil {
		route(patch, runtimeRoutes, "RestaurantRuntimeIdle")
		return nil
	}
	// A confirmed failed click may be retried in the newly shortest kitchen.
	next := ledger.makeDispatch(pending.DishID)
	if next == nil {
		route(patch, runtimeRoutes, "RestaurantRuntimeIdle")
		return nil
	}
	ledger.Pending = next
	setDispatchTarget(next, patch)
	if view.Selected != next.Kitchen {
		route(patch, runtimeRoutes, fmt.Sprintf("RestaurantRuntimeSwitchKitchen%d", next.Kitchen))
	} else {
		route(patch, runtimeRoutes, "RestaurantRuntimeLocateDish")
	}
	return nil
}

func setDispatchTarget(dispatch *Dispatch, patch map[string]any) {
	patch["RestaurantRuntimeTargetKitchen"] = map[string]any{"all_of": []string{fmt.Sprintf("RestaurantKitchen%dSelected", dispatch.Kitchen)}}
	for slot := 1; slot <= 6; slot++ {
		iconCandidates(patch, fmt.Sprintf("RestaurantRecipe%dTargetIcon", slot), []string{dispatch.DishID})
	}
}

func recipeClickSlot(observed func(string) bool) (int, error) {
	for slot := 1; slot <= 6; slot++ {
		if observed(fmt.Sprintf("RestaurantRecipe%dTargetIcon", slot)) {
			return slot, nil
		}
	}
	return 0, fmt.Errorf("target dish slot is uncertain")
}

func prepareDispatch(state *session, detail *maa.RecognitionDetail, observed func(string) bool, box maa.Rect, patch map[string]any) error {
	current := state.Runtime
	if current == nil || current.Ledger.Pending == nil || box[2] <= 0 || box[3] <= 0 {
		return fmt.Errorf("pending dispatch or recipe observation is missing")
	}
	// Refresh dot counts immediately before the click; completions must not be
	// confused with the subsequent addition to the selected kitchen.
	view, err := readKitchenView(observed)
	if err != nil {
		return err
	}
	if _, err := current.Ledger.observeQueues(view, detail, false); err != nil {
		return err
	}
	pending := current.Ledger.Pending
	pending = current.Ledger.makeDispatch(pending.DishID)
	current.Ledger.Pending = pending
	if pending == nil {
		route(patch, runtimeRoutes, "RestaurantRuntimeIdle")
		return nil
	}
	if pending.Kitchen != view.Selected {
		setDispatchTarget(pending, patch)
		route(patch, runtimeRoutes, fmt.Sprintf("RestaurantRuntimeSwitchKitchen%d", pending.Kitchen))
		return nil
	}
	iconCandidates(patch, "RestaurantRuntimeClickDishIcon", []string{pending.DishID})
	patch["RestaurantRuntimeClickDishIcon"].(map[string]any)["roi"] = box
	portions := current.Ledger.counterCount() + 1
	for _, queue := range current.Ledger.Kitchens {
		portions += len(queue)
	}
	frame := "RestaurantRuntimeQueueCounts"
	if portions >= CounterTrayCount {
		frame = "RestaurantRuntimeFullFrame"
	}
	clickSlot, err := recipeClickSlot(observed)
	if err != nil {
		return err
	}
	patch["RestaurantRuntimeVerifyDispatch"] = map[string]any{"all_of": []string{frame, "RestaurantRuntimeQueueIcons"}}
	route(patch, runtimeRoutes, fmt.Sprintf("RestaurantRuntimeClickDish%d", clickSlot))
	return nil
}

// Only item IDs are required; cooks and cooking times are deliberately absent.
type menuCatalog struct {
	Dishes []string `json:"dishes"`
	Drinks []string `json:"drinks"`
}

const (
	KitchenCount      = 3
	KitchenQueueSize  = 3
	CounterTrayCount  = 8
	PrestockTrayLimit = 6
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
	Points       []OrderPoint `json:"points"`
	NeedsRestock []string     `json:"needs_restock"`
}

type Dispatch struct {
	DishID    string `json:"dish_id"`
	Kitchen   int    `json:"kitchen"`
	Uncertain bool   `json:"uncertain,omitempty"`
}

type Ledger struct {
	Kitchens [KitchenCount][]string
	Counter  map[string]int
	Dishes   []string
	DishIDs  map[string]struct{}
	DrinkIDs map[string]struct{}
	Points   map[string]OrderPoint
	Pending  *Dispatch
}

func newLedger(dishes, drinks []string) Ledger {
	ledger := Ledger{
		Counter:  make(map[string]int),
		Dishes:   append([]string(nil), dishes...),
		DishIDs:  make(map[string]struct{}, len(dishes)),
		DrinkIDs: make(map[string]struct{}, len(drinks)),
		Points:   make(map[string]OrderPoint),
	}
	for _, dishID := range dishes {
		ledger.DishIDs[dishID] = struct{}{}
	}
	for _, drinkID := range drinks {
		ledger.DrinkIDs[drinkID] = struct{}{}
	}
	return ledger
}

// A queue may lose only a known FIFO prefix, and a verified click may add one
// dish. Compare every visible slot only when dot counts alone are insufficient.
func queueProgress(queue []string, count int, visible []string, pending string) (int, bool, error) {
	without := len(queue) - count
	with := len(queue) + 1 - count
	withoutMatches := without >= 0 && without <= len(queue) && slices.Equal(queue[without:], visible)
	withMatches := count > 0 && with >= 0 && with <= len(queue) && slices.Equal(append(append([]string(nil), queue[with:]...), pending), visible)
	if withoutMatches && withMatches {
		return 0, false, errAmbiguousQueue
	}
	if withMatches {
		return with, true, nil
	}
	if withoutMatches {
		return without, false, nil
	}
	return 0, false, fmt.Errorf("visible queue %v does not match recorded FIFO %v or pending dish %q", visible, queue, pending)
}

func (l *Ledger) observeQueues(view kitchenQueueView, detail *maa.RecognitionDetail, verify bool) (bool, error) {
	removed := [KitchenCount]int{}
	added := false
	var queueErr error
queues:
	for kitchen, count := range view.Counts {
		queue := l.Kitchens[kitchen]
		removed[kitchen] = len(queue) - count
		if verify && l.Pending != nil && l.Pending.Kitchen == kitchen+1 {
			if view.Selected != kitchen+1 {
				return false, fmt.Errorf("pending kitchen is not selected during verification")
			}
			if count > len(queue)+1 {
				return false, fmt.Errorf("kitchen %d gained more than the pending dish", kitchen+1)
			}
			if count == len(queue)+1 {
				removed[kitchen], added = 0, true
				continue
			}
			visible := make([]string, 0, count)
			for point := 1; point <= count; point++ {
				id, err := readIcon(detail, fmt.Sprintf("RestaurantQueuePoint%dIcon", point))
				if err != nil || id == "" {
					l.Pending.Uncertain = true
					queueErr = fmt.Errorf("queue point %d is unrecognized: %v", point, err)
					continue queues
				}
				visible = append(visible, id)
			}
			completed, accepted, err := queueProgress(queue, count, visible, l.Pending.DishID)
			if err != nil {
				l.Pending.Uncertain = true
				queueErr = err
				continue
			}
			removed[kitchen], added = completed, accepted
			if l.Pending.Uncertain && !added && count == 0 {
				// Later empty slots do not prove that an earlier ambiguous click failed.
				queueErr = errAmbiguousQueue
			} else if !added {
				// A nonempty old FIFO proves that the new tail was not added.
				l.Pending.Uncertain = false
			}
		} else if count > len(queue) {
			return false, fmt.Errorf("kitchen %d has unrecorded queue entries", kitchen+1)
		}
	}
	for kitchen, count := range removed {
		for _, dishID := range l.Kitchens[kitchen][:count] {
			l.Counter[dishID]++
			log.Debug().Int("kitchen", kitchen+1).Str("dish_id", dishID).Msg("queue dots decreased; dish moved to counter")
		}
		l.Kitchens[kitchen] = l.Kitchens[kitchen][count:]
	}
	if added {
		pending := l.Pending
		l.Kitchens[pending.Kitchen-1] = append(l.Kitchens[pending.Kitchen-1], pending.DishID)
		l.Pending = nil
	}
	return added, queueErr
}

func (l *Ledger) observeTables(snapshot GameSnapshot) error {
	current := make(map[string]OrderPoint, len(snapshot.Points))
	for _, point := range snapshot.Points {
		if point.Exists {
			if _, dish := l.DishIDs[point.ItemID]; !dish {
				if _, drink := l.DrinkIDs[point.ItemID]; !drink {
					return fmt.Errorf("table point %d/%d has unselected item %q", point.Key.Table, point.Key.Point, point.ItemID)
				}
			}
		}
		current[orderPointStorageKey(point.Key)] = point
	}
	for _, dishID := range snapshot.NeedsRestock {
		if _, selected := l.DishIDs[dishID]; !selected {
			return fmt.Errorf("supplement marker belongs to unselected dish %q", dishID)
		}
	}
	consumed := make(map[string]int)
	for key, previous := range l.Points {
		next, exists := current[key]
		if !exists || !next.Exists || next.ItemID != previous.ItemID {
			if previous.Exists && !previous.Served {
				if _, isDrink := l.DrinkIDs[previous.ItemID]; !isDrink {
					return fmt.Errorf("dish point %d/%d changed before its served marker was observed", previous.Key.Table, previous.Key.Point)
				}
			}
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
			return fmt.Errorf("served dish %q needs %d portions but counter ledger holds %d", dishID, quantity, l.Counter[dishID])
		}
	}
	for dishID, quantity := range consumed {
		l.Counter[dishID] -= quantity
	}
	l.Points = current
	return nil
}

func orderPointStorageKey(key OrderPointKey) string {
	return fmt.Sprintf("%d/%d", key.Table, key.Point)
}

func (l *Ledger) suggestDispatch(needsRestock []string) *Dispatch {
	marked := make(map[string]struct{}, len(needsRestock))
	for _, dishID := range needsRestock {
		marked[dishID] = struct{}{}
	}
	for _, dishID := range l.Dishes {
		if _, needs := marked[dishID]; !needs {
			continue
		}
		return l.makeDispatch(dishID)
	}
	for _, dishID := range l.Dishes {
		if l.Counter[dishID]+l.inFlight(dishID) >= l.unservedDemand(dishID) {
			continue
		}
		return l.makeDispatch(dishID)
	}
	for _, dishID := range l.Dishes {
		if l.Counter[dishID]+l.inFlight(dishID) > 0 {
			continue
		}
		return l.makeDispatch(dishID)
	}
	return nil
}

// Stable iteration implements kitchen 1 > 2 > 3 when queue lengths tie.
func (l *Ledger) makeDispatch(dishID string) *Dispatch {
	if l.counterCount() >= CounterTrayCount {
		return nil
	}
	selected := -1
	for kitchen, queue := range l.Kitchens {
		if len(queue) < KitchenQueueSize && (selected < 0 || len(queue) < len(l.Kitchens[selected])) {
			selected = kitchen
		}
	}
	if selected < 0 {
		return nil
	}
	return &Dispatch{DishID: dishID, Kitchen: selected + 1}
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
			if job == dishID {
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
