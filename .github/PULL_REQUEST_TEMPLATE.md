## Summary

Describe the problem and the behavior changed by this pull request.

## Related Issue

Fixes #

## Verification

List the exact commands and environments used to verify the change.

```text
make verify
```

## Checklist

- [ ] The change is scoped to one concern and includes tests or a reason tests are not needed.
- [ ] Provider-specific behavior stays behind a provider adapter.
- [ ] Retries and controller restarts cannot create duplicate paid resources.
- [ ] Delete and failure paths have been considered, including lost responses and manual remote deletion.
- [ ] Unsupported Pod fields fail explicitly instead of being silently ignored.
- [ ] Manifests, RBAC, examples, and documentation are updated when their behavior changes.
- [ ] No credentials, private endpoints, customer identifiers, or paid resource IDs are included.
- [ ] Any real-provider test names its cost guard and confirms cleanup.
