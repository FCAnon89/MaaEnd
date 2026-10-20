package restaurant

import maa "github.com/MaaXYZ/maa-framework-go/v4"

func Register() {
	maa.AgentServerRegisterCustomRecognition("RestaurantKitchenOperatorsRecognition", &KitchenOperatorsRecognition{})
	maa.AgentServerRegisterCustomRecognition("RestaurantMenuInitializeRecognition", &MenuInitializeRecognition{})
	maa.AgentServerRegisterCustomRecognition("RestaurantMenuPlanRecognition", &MenuPlanRecognition{})
	maa.AgentServerRegisterCustomRecognition("RestaurantMenuPlanFailureRecognition", &MenuPlanFailureRecognition{})
	maa.AgentServerRegisterCustomRecognition("RestaurantMenuNextSelectionRecognition", &MenuNextSelectionRecognition{})
	maa.AgentServerRegisterCustomRecognition("RestaurantMenuSelectionVerifiedRecognition", &MenuSelectionVerifiedRecognition{})
	maa.AgentServerRegisterCustomRecognition("RestaurantMenuSelectionCompleteRecognition", &MenuSelectionCompleteRecognition{})
	maa.AgentServerRegisterCustomRecognition("RestaurantMenuSelectionFailureRecognition", &MenuSelectionFailureRecognition{})
	maa.AgentServerRegisterCustomRecognition("RestaurantMenuSelectionBottomRecognition", &MenuSelectionBottomRecognition{})
	maa.AgentServerRegisterCustomRecognition("RestaurantRuntimeNextKitchenRecognition", &RuntimeNextKitchenRecognition{})
	maa.AgentServerRegisterCustomRecognition("RestaurantRuntimeNextDishRecognition", &RuntimeNextDishRecognition{})
	maa.AgentServerRegisterCustomRecognition("RestaurantRuntimeVerifyDispatchRecognition", &RuntimeVerifyDispatchRecognition{})
	maa.AgentServerRegisterCustomRecognition("RestaurantRuntimeFailureRecognition", &RuntimeFailureRecognition{})
}
