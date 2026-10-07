package dijiangrewards

import maa "github.com/MaaXYZ/maa-framework-go/v4"

func Register() {
	maa.AgentServerRegisterCustomRecognition(recoveryEmotionOperatorRecognitionName, &recoveryEmotionOperatorRecognitionRunner{})
	maa.AgentServerRegisterCustomRecognition(countdownComponent, &ExchangeCountdownWithinThresholdRecognition{})
	maa.AgentServerRegisterCustomRecognition(keepAliveComponent, &ExchangeKeepAliveDueRecognition{})
}
