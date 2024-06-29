package reviewapp

import (
	racwilliamnuv1alpha1 "github.com/wille/rac/api/v1alpha1"
)

type UpdateConfig struct {
	Image string `json:"image"`
}

func Update(reviewApp *racwilliamnuv1alpha1.ReviewApp, config *UpdateConfig) {

}
