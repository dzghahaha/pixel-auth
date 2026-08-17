# Pixel Auth Project Instructions

## Testing Guidelines
- **Integration Test Isolation**: The test suite (`main_test.go`) connects to a remote test database and drops/recreates all tables on `initTestDB(t)` for isolation.
- **Selective Test Execution**: During development, refactoring, or validation, **never** run the entire test suite (`go test ./...` or `go test -v`) as parallel execution/collisions on the shared remote test DB will cause `Table doesn't exist` or duplicate key errors.
- **Action**: Always run only the specific test case you are modifying or validating using the `-run` flag, e.g.:
  ```bash
  go test -v -run TestCancelSubscription
  ```
