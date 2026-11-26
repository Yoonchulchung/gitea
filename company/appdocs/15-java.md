# Java 앱 — Spring Boot
> pom.xml 로 만드는 Spring Boot 앱의 빌드·실행·데이터 규칙.
keywords: java, spring, spring boot, springboot, pom.xml, maven, mvn, jar, tomcat, jpa, 자바, 스프링, 메이븐

## 빌드와 실행
- 저장소 루트의 `pom.xml`을 Maven으로 `package`(테스트는 건너뜀)하고, 만들어진 Spring Boot jar를 서버의 Java로 실행한다. Java 17 이상, Spring Boot 3 이상으로 쓴다. Gradle은 아직 지원하지 않는다.
- `spring-boot-starter-web`(Tomcat)과 `spring-boot-maven-plugin`이 있어야 한다. WebFlux(Netty)·Jetty·Undertow는 지원하지 않는다.
- 포트를 설정하지 않는다. 플랫폼이 빌드할 때 설정 클래스(`company.platform.PlatformSocketConfiguration`)를 넣어 Tomcat이 앱 소켓에서 듣게 한다. 그 이름의 클래스를 만들지 않는다.
- 메모리 한도는 기본 512MB이고 힙은 그 60%로 잡힌다. 시작 직후 `/health`를 확인하므로 `@GetMapping("/health")`가 응답하게 한다.
- 데이터는 Python 앱과 같다: SQLite 하나, `jdbc:sqlite:` + `DB_PATH` 환경 변수(`org.xerial:sqlite-jdbc`). 테이블은 `migrations/001_*.sql` 파일로 만들고, JPA를 쓰면 `spring.jpa.hibernate.ddl-auto=none`으로 둔다.
- `org.springframework.boot`의 라이브러리는 승인 없이 쓴다. 그 밖에 `pom.xml`에 직접 적은 라이브러리는 배포 요청에서 승인을 받는다.
- 링크는 Python 앱과 같이 상대 경로로 쓴다. Thymeleaf의 `@{/x}`는 플랫폼이 넘기는 앱 경로를 자동으로 붙인다.
- Java 앱의 외부 통신(허용 목록)은 아직 지원하지 않는다.
