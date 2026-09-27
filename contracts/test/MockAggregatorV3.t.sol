// SPDX-License-Identifier: MIT
pragma solidity ^0.8.20;

import {Test} from "forge-std/Test.sol";
import {MockAggregatorV3} from "../src/mocks/MockAggregatorV3.sol";

contract MockAggregatorV3Test is Test {
    function test_setAnswerAdvancesRound() public {
        MockAggregatorV3 feed = new MockAggregatorV3("BTC / USD", 100_000e8);
        assertEq(feed.decimals(), 8);

        (uint80 round, int256 answer,,, uint80 answeredIn) = feed.latestRoundData();
        assertEq(round, 1);
        assertEq(answer, 100_000e8);
        assertEq(answeredIn, 1);

        vm.warp(block.timestamp + 60);
        feed.setAnswer(95_000e8);
        uint256 updatedAt;
        (round, answer,, updatedAt,) = feed.latestRoundData();
        assertEq(round, 2);
        assertEq(answer, 95_000e8);
        assertEq(updatedAt, block.timestamp);

        (, answer,,,) = feed.getRoundData(1);
        assertEq(answer, 100_000e8);
    }
}
