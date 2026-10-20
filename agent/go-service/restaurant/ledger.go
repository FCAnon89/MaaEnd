package restaurant

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

const (
	KitchenCount      = 3
	KitchenQueueSize  = 3
	CounterTrayCount  = 8
	PrestockTrayLimit = 6
)

var closingCountdownPattern = regexp.MustCompile(`^[^0-9:]*营业[^0-9:]*([0-9]{1,2})[^0-9:]*$`)

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
