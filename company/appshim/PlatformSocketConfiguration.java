// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company.platform;

import java.lang.reflect.Array;
import java.lang.reflect.Method;
import java.lang.reflect.Proxy;

import org.springframework.beans.factory.config.BeanPostProcessor;
import org.springframework.context.annotation.Bean;
import org.springframework.context.annotation.Configuration;

/**
 * Added by the platform when it builds a Spring Boot app: puts the embedded
 * Tomcat on the unix socket named by the SOCKET environment variable, which
 * is how the platform reaches every app. The connector customizer type moved
 * package between Spring Boot versions, so it is found by reflection and this
 * file compiles with whichever version the app uses.
 */
@Configuration(proxyBeanMethods = false)
public class PlatformSocketConfiguration {

	@Bean
	public static BeanPostProcessor platformSocketCustomizer() {
		return new BeanPostProcessor() {
			@Override
			public Object postProcessBeforeInitialization(Object bean, String beanName) {
				String socket = System.getenv("SOCKET");
				if (socket == null || socket.isEmpty()) {
					return bean; // run outside the platform: keep the usual port
				}
				for (Method add : bean.getClass().getMethods()) {
					if (!add.getName().equals("addConnectorCustomizers") || add.getParameterCount() != 1
							|| !add.getParameterTypes()[0].isArray()) {
						continue;
					}
					Class<?> type = add.getParameterTypes()[0].getComponentType();
					Object customizer = Proxy.newProxyInstance(type.getClassLoader(), new Class<?>[] {type},
							(proxy, method, args) -> {
								switch (method.getName()) {
									case "customize":
										args[0].getClass().getMethod("setProperty", String.class, String.class)
												.invoke(args[0], "unixDomainSocketPath", socket);
										return null;
									case "equals":
										return proxy == args[0];
									case "hashCode":
										return System.identityHashCode(proxy);
									default:
										return "platform socket " + socket;
								}
							});
					Object customizers = Array.newInstance(type, 1);
					Array.set(customizers, 0, customizer);
					try {
						add.invoke(bean, customizers);
					} catch (ReflectiveOperationException e) {
						throw new IllegalStateException("could not put Tomcat on the platform socket", e);
					}
					break;
				}
				return bean;
			}
		};
	}
}
