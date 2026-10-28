package restaurant

import "testing"

func TestPrepareMenuRequirements(t *testing.T) {
	tests := []struct {
		name       string
		text       string
		wantDishes int
		wantDrinks int
		wantError  bool
	}{
		{name: "valid requirements", text: "至少需要6道菜品和3种饮品", wantDishes: 6, wantDrinks: 3},
		{name: "drink count overflow", text: "6道菜品和999999999999999999999999999999种饮品", wantError: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			state := session{}
			err := prepareMenu(&state, test.text)
			if (err != nil) != test.wantError {
				t.Fatalf("prepareMenu() error = %v, wantError %t", err, test.wantError)
			}
			if err == nil && (state.Menu.MinimumDishes != test.wantDishes || state.Menu.MinimumDrinks != test.wantDrinks) {
				t.Fatalf("prepareMenu() requirements = (%d, %d), want (%d, %d)", state.Menu.MinimumDishes, state.Menu.MinimumDrinks, test.wantDishes, test.wantDrinks)
			}
		})
	}
}
