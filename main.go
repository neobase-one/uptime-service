package main

import (
	"flag"
	"log"
	"math"

	"uptime-service/aggregator"
	"uptime-service/config"
	"uptime-service/contract"
	"uptime-service/db"
	"uptime-service/delegation"
	"uptime-service/errutil"
	"uptime-service/logging"
	"uptime-service/validator"

	"github.com/ava-labs/avalanchego/ids"
	avalancheWarp "github.com/ava-labs/avalanchego/vms/platformvm/warp"
)

func main() {
	configPath := flag.String("config", "config.json", "Path to config file")
	flag.Parse()

	if len(flag.Args()) == 0 {
		log.Fatalf("Command required: 'generate' or 'submit'")
	}

	cmd := flag.Args()[0]
	if cmd != "generate" && cmd != "submit" {
		log.Fatalf("Unknown command: %s. Must be 'generate' or 'submit'", cmd)
	}

	cfg, err := config.LoadConfig(*configPath)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	logging.SetLevel(cfg.LogLevel)

	dbClient, err := db.NewDBClient(cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("Failed to initialize database client: %v", err)
	}
	defer dbClient.Close()

	switch cmd {
	case "generate":
		err = generateUptimeProofs(cfg, dbClient)
	case "submit":
		err = submitUptimeProofs(cfg, dbClient)
	}

	if err != nil {
		log.Fatalf("Command %s failed: %v", cmd, err)
	}
	logging.Info("Command completed successfully")
}

// generateUptimeProofs fetches validator uptimes, generates signatures, and stores them in the db
func generateUptimeProofs(cfg *config.Config, dbClient *db.DBClient) error {
	aggClient, err := aggregator.NewAggregatorClient(cfg.AggregatorURL, uint32(cfg.NetworkID), cfg.SigningSubnetID, cfg.SourceChainId, cfg.LogLevel)
	if err != nil {
		return err
	}

	// 1. Fetch current validators and their uptime from Avalanche P-Chain
	validators, err := validator.FetchUptimes(cfg.AvalancheAPI)
	if err != nil {
		logging.Errorf("Error fetching validator uptimes: %v", err)
		return err
	}
	logging.Infof("Fetched %d validators' uptime info", len(validators))

	// 2. Filter out inactive validators and log them
	inactiveValidators := []string{}
	activeValidators := make([]validator.ValidatorUptime, 0, len(validators))

	for _, val := range validators {
		if val.IsActive {
			activeValidators = append(activeValidators, val)
		} else {
			inactiveValidators = append(inactiveValidators, val.NodeID)
		}
	}

	if len(inactiveValidators) > 0 {
		logging.Infof("Filtered out %d inactive validators", len(inactiveValidators))
		logging.Info("Inactive NodeIDs:")
      for _, nodeID := range inactiveValidators {
        logging.Info(" - " + nodeID)
      }
	}

	// 3. For each active validator, build message and aggregate signatures
	for _, val := range activeValidators {
		// Parse the validation ID into a 32-byte array
		validationIDBytes, err := ids.FromString(val.ValidationID)
		if errutil.HandleError("parsing validation ID for "+val.ValidationID, err) {
			continue
		}
		validationID := ids.ID(validationIDBytes)

		var successfulUptimeSeconds uint64
		var successfulSignedMsg *avalancheWarp.Message

		// First try with the reported uptime seconds
		currentUptimeSeconds := val.UptimeSeconds
		unsignedMsg, err := aggClient.PackValidationUptimeMessage(val.ValidationID, currentUptimeSeconds, uint32(cfg.NetworkID))
		if errutil.HandleError("building initial uptime message for "+val.ValidationID, err) {
			continue
		}

		logging.Infof("Built uptime message for validator %s (uptime=%d seconds)", val.ValidationID, currentUptimeSeconds)

		signedMsg, err := aggClient.SubmitAggregateRequest(unsignedMsg)

		if err != nil {
			// Path 1: Initial attempt failed, try decreasing by 5% until it succeeds
			logging.Infof("Initial signature attempt failed for %s. Trying with decreasing uptime values...", val.ValidationID)

			for {
				// Decrease by 5%, ensuring we get an integer
				currentUptimeSeconds = uint64(math.Floor(float64(currentUptimeSeconds) * 0.95))

				if currentUptimeSeconds == 0 {
					logging.Errorf("Uptime seconds reached zero for %s, aborting retry", val.ValidationID)
					break
				}

				logging.Infof("Trying with decreased uptime = %d seconds for %s", currentUptimeSeconds, val.ValidationID)

				unsignedMsg, err := aggClient.PackValidationUptimeMessage(val.ValidationID, currentUptimeSeconds, uint32(cfg.NetworkID))
				if errutil.HandleError("building decreased uptime message for "+val.ValidationID, err) {
					break
				}

				signedMsg, err = aggClient.SubmitAggregateRequest(unsignedMsg)
				if err == nil {
					// Success with decreased value
					successfulUptimeSeconds = currentUptimeSeconds
					successfulSignedMsg = signedMsg
					logging.Infof("Success with decreased uptime = %d seconds for %s", currentUptimeSeconds, val.ValidationID)
					break
				}

				logging.Infof("Still failed with uptime = %d seconds, continuing to decrease", currentUptimeSeconds)
			}
		} else {
			// Path 2: Initial attempt succeeded, try increasing by 5% until it fails
			logging.Infof("Initial signature attempt succeeded for %s. Trying with increasing uptime values...", val.ValidationID)
			successfulUptimeSeconds = currentUptimeSeconds
			successfulSignedMsg = signedMsg

			for {
				// Increase by 5%, ensuring we get an integer
				nextUptimeSeconds := uint64(math.Ceil(float64(currentUptimeSeconds) * 1.05))

				// Safety check to ensure we actually increased (for small values)
				if nextUptimeSeconds <= currentUptimeSeconds {
					nextUptimeSeconds = currentUptimeSeconds + 1
				}

				logging.Infof("Trying with increased uptime = %d seconds for %s", nextUptimeSeconds, val.ValidationID)

				unsignedMsg, err := aggClient.PackValidationUptimeMessage(val.ValidationID, nextUptimeSeconds, uint32(cfg.NetworkID))
				if errutil.HandleError("building increased uptime message for "+val.ValidationID, err) {
					break
				}

				tempSignedMsg, err := aggClient.SubmitAggregateRequest(unsignedMsg)
				if err != nil {
					// Failed with increased value, keep the last successful values
					logging.Infof("Failed with increased uptime = %d seconds, using last successful value = %d seconds",
						nextUptimeSeconds, successfulUptimeSeconds)
					break
				}

				currentUptimeSeconds = nextUptimeSeconds
				successfulUptimeSeconds = currentUptimeSeconds
				successfulSignedMsg = tempSignedMsg
				logging.Infof("Success with increased uptime = %d seconds, continuing to increase", successfulUptimeSeconds)
			}
		}

		// Check if we have a successful signed message
		if successfulSignedMsg == nil {
			logging.Errorf("Failed to get any successful signature for validator %s, skipping", val.ValidationID)
			continue
		}

		logging.Infof("Storing uptime = %d seconds for validator %s in database", successfulUptimeSeconds, val.ValidationID)

		// Store the uptime proof in the database
		err = dbClient.StoreUptimeProof(validationID, successfulUptimeSeconds, successfulSignedMsg)
		if errutil.HandleError("storing uptime proof for "+val.ValidationID, err) {
			continue
		}
		logging.Infof("Successfully stored uptime proof for validator %s in database", val.ValidationID)
	}

	return nil
}

// submitUptimeProofs submits uptime proofs from the database to the smart contract and resolves rewards
func submitUptimeProofs(cfg *config.Config, dbClient *db.DBClient) error {
	contractClient, err := contract.NewContractClient(cfg.BeamRPC, cfg.StakingManagerAddress, cfg.WarpMessengerAddress, cfg.PrivateKey)
	if err != nil {
		return err
	}
	logging.Infof("Connected to staking manager contract at %s", cfg.StakingManagerAddress)

	delegationClient, err := delegation.NewDelegationClient(
		cfg.GraphQLEndpoint,
		cfg.BeamRPC,
		cfg.StakingManagerAddress,
		cfg.PrivateKey,
	)
	if err != nil {
		return err
	}
	logging.Info("Connected to delegation service")

	// Get all uptime proofs from the database
	proofs, err := dbClient.GetAllUptimeProofs()
	if err != nil {
		return err
	}

	if len(proofs) == 0 {
		logging.Info("No uptime proofs found in database")
		return nil
	}

	logging.Infof("Found %d uptime proofs in database", len(proofs))

	// Track validators with successful uptime submissions
	successfulValidators := make([]string, 0)

	for validationIDStr, proof := range proofs {
		validationID := proof.ValidationID
		signedMessage := proof.SignedMessage
		uptimeSeconds := proof.UptimeSeconds

		// Submit the signed uptime proof to the smart contract
		err = contractClient.SubmitUptimeProof(validationID, signedMessage)
		if errutil.HandleError("submitting uptime proof for "+validationIDStr, err) {
			continue
		}

		logging.Infof("Submitted uptime proof for validator %s to contract with uptime %d seconds", validationIDStr, uptimeSeconds)

		// Add this validator to the successful list for delegation resolution
		successfulValidators = append(successfulValidators, validationIDStr)
	}

	// For each successful validator, fetch and resolve delegations
	logging.Infof("Processing delegations for %d successful validators", len(successfulValidators))
	for _, validationID := range successfulValidators {
		// Fetch delegations for this validator
		delegations, err := delegationClient.GetDelegationsForValidator(validationID)
		if errutil.HandleError("fetching delegations for "+validationID, err) {
			continue
		}

		logging.Infof("Found %d delegations for validator %s", len(delegations), validationID)

		// Resolve rewards for these delegations
		if len(delegations) > 0 {
			err = delegationClient.ResolveRewards(delegations)
			if errutil.HandleError("resolving rewards for validator "+validationID, err) {
				continue
			}
			logging.Infof("Successfully resolved rewards for %d delegations of validator %s", len(delegations), validationID)
		}
	}

	return nil
}
