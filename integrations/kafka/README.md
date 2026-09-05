# XBC Kafka integration

This independent module pins `kafka-go` and keeps it out of XBC core. Each key
below `plugins.kafka` creates a named `*kafka.Client` and exports the
`kafka.Producer` contract. Applications wire either contract with typed plugin
inputs.

Consumer handlers are registered by configured consumer name during dependent
plugin factories and freeze at lifecycle `Start`. `Start` creates readers and
submits every consumer as a critical managed task; each task waits for the
application-wide traffic gate before fetching. Commits are synchronous so retry
and `error_policy` can observe broker failures. Only
`integrations/kafka/autoload` contributes to optional process-wide autoload
composition; ordinary imports and `Bundle()` are side-effect free.
