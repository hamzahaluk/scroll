# Permissionless Batches
Permissionless batches aka enforced batches is a feature that provides guarantee to users that they can exit Scroll even if the operator is down or censoring. 
It allows anyone to take over and submit a batch (permissionless batch submission) together with a proof after a certain time period has passed without a batch being finalized on L1. 

Once permissionless batch mode is activated, the operator can no longer submit batches in a permissioned way. Only the security council can deactivate permissionless batch mode and reinstate the operator as the only batch submitter.
There are two types of situations to consider:
- `Permissionless batch mode is activated:` This means that finalization halted for some time. Now anyone can submit batches utilizing the [batch production toolkit](#batch-production-toolkit).
- `Permissionless batch mode is deactivated:` This means that the security council has decided to reinstate the operator as the only batch submitter. The operator needs to [recover](#operator-recovery) the sequencer and relayer to resume batch submission and the valid L2 chain.


## Pre-requisites
- install instructions
- download stuff for coordinator


## Batch production toolkit
1. l2geth recovery 
2. l2geth block production 
3. relayer in permissionless mode with proving etc

### Proving service
```
"l2geth": {
    "endpoint": ""
  }
```


## Operator recovery
- l2geth recovery and relayer recovery

### Relayer
```
l2_config.endpoint
```

