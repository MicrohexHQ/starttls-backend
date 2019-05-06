package validator

import (
	"fmt"
	"log"
	"time"

	"github.com/EFForg/starttls-backend/checker"
	"github.com/EFForg/starttls-backend/models"
	"github.com/getsentry/raven-go"
)

// DomainPolicyStore is an interface for any back-end that
// stores a map of domains to its "policy" (in this case, just the
// expected hostnames).
type DomainPolicyStore interface {
	DomainsToValidate() ([]string, error)
	GetDomain(string) (models.Domain, error)
}

// Called with failure by defaault.
func reportToSentry(name string, domain string, result checker.DomainResult) {
	raven.CaptureMessageAndWait("Validation failed for previously validated domain",
		map[string]string{
			"validatorName": name,
			"domain":        result.Domain,
			"status":        fmt.Sprintf("%d", result.Status),
		},
		result)
}

type resultCallback func(string, models.Domain, checker.DomainResult)

// CheckPerformer defines a function that performs a security check on a domain.
type CheckPerformer func(models.Domain) checker.DomainResult

// Validator runs checks regularly against domain policies. This structure
// defines the configurations.
type Validator struct {
	// Name: Required with which to refer to this validator. Appears in log files and
	// error reports.
	Name string
	// Store: Required-- store from which the validator fetches policies to validate.
	Store DomainPolicyStore
	// Interval: optional; time at which validator should re-run.
	// If not set, default interval is 1 day.
	Interval time.Duration
	// OnFailure: optional. Called when a particular policy validation fails. Defaults to
	// a sentry report.
	OnFailure resultCallback
	// OnSuccess: optional. Called when a particular policy validation succeeds.
	OnSuccess resultCallback
	// CheckPerformer: performs the check.
	CheckPerformer CheckPerformer
}

// UpdatePolicy is a callback we can provide to GetDBCheck in order to perform a policy
// update if we notice a discrepancy between our view and the MTA-STS policy.
type UpdatePolicy func(models.Domain) error

// GetDBCheck returns a CheckPerformer that performs an MTASTS check and update if
// the policy is updated, or performs a regular security check if MTASTS is not supported.
func GetDBCheck(update UpdatePolicy) CheckPerformer {
	c := checker.Checker{Cache: checker.MakeSimpleCache(time.Hour)}
	return func(domain models.Domain) checker.DomainResult {
		if domain.MTASTS {
			result := c.CheckDomain(domain.Name, []string{})
			if !domain.SamePolicy(result.MTASTSResult) {
				if update(domain) != nil {
					reportToSentry("Couldn't update policy in DB", domain.Name, result)
				}
			}
			return result
		}
		return c.CheckDomain(domain.Name, domain.MXs)
	}
}

func (v *Validator) checkPolicy(domain models.Domain) checker.DomainResult {
	if v.CheckPerformer == nil {
		c := checker.Checker{
			Cache: checker.MakeSimpleCache(time.Hour),
		}
		v.CheckPerformer = func(domain models.Domain) checker.DomainResult {
			return c.CheckDomain(domain.Name, domain.MXs)
		}
	}
	return v.CheckPerformer(domain)
}

func (v *Validator) interval() time.Duration {
	if v.Interval != 0 {
		return v.Interval
	}
	return time.Hour * 24
}

func (v *Validator) policyFailed(name string, domain models.Domain, result checker.DomainResult) {
	if v.OnFailure != nil {
		v.OnFailure(name, domain, result)
	}
	reportToSentry(name, domain.Name, result)
}

func (v *Validator) policyPassed(name string, domain models.Domain, result checker.DomainResult) {
	if v.OnSuccess != nil {
		v.OnSuccess(name, domain, result)
	}
}

// Run starts the endless loop of validations. The first validation happens after the given
// Interval. Validation failures induce `policyFailed`, and successes cause `policyPassed`.
func (v *Validator) Run() {
	for {
		<-time.After(v.interval())
		log.Printf("[%s validator] starting regular validation", v.Name)
		domains, err := v.Store.DomainsToValidate()
		if err != nil {
			log.Printf("[%s validator] Could not retrieve domains: %v", v.Name, err)
			continue
		}
		for _, domain := range domains {
			domainData, err := v.Store.GetDomain(domain)
			if err != nil {
				log.Printf("[%s validator] Could not retrieve policy for domain %s: %v", v.Name, domain, err)
				continue
			}
			result := v.checkPolicy(domainData)
			if result.Status != 0 {
				log.Printf("[%s validator] %s failed; sending report", v.Name, domain)
				v.policyFailed(v.Name, domainData, result)
			} else {
				v.policyPassed(v.Name, domainData, result)
			}
		}
	}
}

// ValidateRegularly regularly runs checker.CheckDomain against a Domain-
// Hostname map. Interval specifies the interval to wait between each run.
// Failures are reported to Sentry.
func ValidateRegularly(name string, store DomainPolicyStore, interval time.Duration) {
	v := Validator{
		Name:     name,
		Store:    store,
		Interval: interval,
	}
	v.Run()
}
