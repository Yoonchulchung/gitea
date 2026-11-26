# App runtimes

What an app is built and run as is read from its files at deploy time.

| Files at the top of the repository | Runs as |
|---|---|
| `main.py` | A Python web app: `requirements.txt` is installed and the FastAPI `app` in `main.py` is started. |
| `pom.xml`, and no `main.py` | A Java web app: built with Maven and run as a Spring Boot jar. |
| `index.html`, and neither of the above | A static website: the files are served as they are. |

For a static website, `index.html` is looked for in `dist/`, `build/`, the top of the repository, and `public/`, in that order, so a project that commits its build output is served from it. A path that names no file and has no extension serves `index.html`, which is how a single-page app's own routes load. Files whose names start with `.` are never served.

A static website has no server code and no database. It is still started, stopped, limited and health-checked like any other app, and it runs in the same sandbox.

## Java apps

A Java app is a Spring Boot web app (`spring-boot-starter-web` and `spring-boot-maven-plugin`, Java 17 or later). Maven builds it with tests skipped, inside the sandbox, and the Spring Boot jar it makes is run. Gradle, WebFlux, Jetty and Undertow are not supported yet, nor is outbound access through the allow list.

- No port is configured: the build adds a small configuration class that puts Tomcat on the platform's socket.
- Memory defaults to 512 MB and processes to 256, unless an app has its own limits; the heap is 60% of the memory limit.
- Data works as for Python apps: one SQLite database at `DB_PATH`, tables from `migrations/*.sql`.
- Libraries from `org.springframework.boot` need no approval. Others written directly in `pom.xml` are approved like Python packages; what they pull in is not asked about.

The server needs a JDK and Maven (`[company] JAVA_PATH` and `MAVEN_PATH` if they are not on the path). Libraries come from `[company] MAVEN_REPOSITORY_URL`, or from Maven Central only when `MAVEN_ALLOW_PUBLIC_REPOSITORY = true`; otherwise Java deploys are refused.

The code editor's guide lists the kinds of app this server can deploy, with the Python and framework versions it actually runs.
